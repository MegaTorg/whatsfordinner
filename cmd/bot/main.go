package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/korjavin/whatsfordinner/pkg/config"
	"github.com/korjavin/whatsfordinner/pkg/dinner"
	"github.com/korjavin/whatsfordinner/pkg/fridge"
	"github.com/korjavin/whatsfordinner/pkg/logger"
	"github.com/korjavin/whatsfordinner/pkg/messages"
	"github.com/korjavin/whatsfordinner/pkg/models"
	"github.com/korjavin/whatsfordinner/pkg/openai"
	"github.com/korjavin/whatsfordinner/pkg/poll"
	"github.com/korjavin/whatsfordinner/pkg/state"
	"github.com/korjavin/whatsfordinner/pkg/storage"
	"github.com/korjavin/whatsfordinner/pkg/suggest"
	"github.com/korjavin/whatsfordinner/pkg/telegram"
)

func main() {
	// Initialize logger
	log := logger.Global
	log.Info("Starting WhatsForDinner bot...")

	// Load configuration
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Error("Failed to load configuration: %v", err)
		os.Exit(1)
	}

	// Initialize storage
	dataDir := filepath.Join(".", "data")
	store, err := storage.New(dataDir)
	if err != nil {
		log.Error("Failed to initialize storage: %v", err)
		os.Exit(1)
	}
	defer store.Close()

	// Start BadgerDB garbage collection
	store.StartGCRoutine(10 * time.Minute)

	// Initialize OpenAI client
	openaiClient := openai.New(cfg.OpenAIAPIKey, cfg.OpenAIAPIBase, cfg.OpenAIModel)

	// Initialize services
	fridgeService := fridge.New(store)
	// We're not using dinnerService directly anymore, using OpenAI client instead
	// dinnerService := dinner.New(store, fridgeService, openaiClient)
	pollService := poll.New(store)
	messageService := messages.New(openaiClient)
	stateManager := state.New()
	suggestService := suggest.New(store)

	// Initialize Telegram bot
	bot, err := telegram.New(cfg.BotToken)
	if err != nil {
		log.Error("Failed to initialize Telegram bot: %v", err)
		os.Exit(1)
	}

	// Setup command handlers
	commandHandlers := map[string]telegram.CommandHandler{
		"start": func(message *tgbotapi.Message) {
			welcomeMsg := messageService.GenerateWelcomeMessage()
			bot.SendMessage(message.Chat.ID, welcomeMsg)
		},
		"dinner": func(message *tgbotapi.Message) {
			// Start dinner suggestion flow
			chatID := message.Chat.ID

			// Get ingredients from the fridge
			ingredients, err := fridgeService.ListIngredients(chatID)
			if err != nil {
				log.Error("Failed to list ingredients: %v", err)
				errorMsg := messageService.GenerateErrorMessage("retrieve fridge contents")
				bot.SendMessage(chatID, errorMsg)
				return
			}

			if len(ingredients) == 0 {
				bot.SendMessage(chatID, "😢 Your fridge is empty! Please add some ingredients with /sync_fridge or /add_photo before I can suggest dinner options.")
				return
			}

			// Extract ingredient names
			ingredientNames := make([]string, len(ingredients))
			for i, ingredient := range ingredients {
				ingredientNames[i] = ingredient.Name
			}

			// Send a processing message
			processingMsg, _ := bot.SendMessage(chatID, "🧐 Thinking about dinner options based on your ingredients... This might take a moment.")

			// Get user suggestions
			userSuggestions, err := suggestService.GetUnusedSuggestions(chatID)
			if err != nil {
				log.Error("Failed to get user suggestions: %v", err)
				// Continue without user suggestions
				userSuggestions = []*models.SuggestedDish{}
			}

			// Determine how many AI suggestions to get
			aiSuggestionCount := 4
			if len(userSuggestions) > 0 {
				// If we have user suggestions, get fewer AI suggestions
				aiSuggestionCount = 4 - len(userSuggestions)
				if aiSuggestionCount < 2 {
					aiSuggestionCount = 2 // Always get at least 2 AI suggestions
				}
			}

			// Get dinner suggestions from OpenAI
			aiSuggestions, err := openaiClient.SuggestDinnerOptions(ingredientNames, cfg.Cuisines, aiSuggestionCount)
			if err != nil {
				log.Error("Failed to get dinner suggestions: %v", err)

				// If we have user suggestions, continue with those
				if len(userSuggestions) == 0 {
					bot.EditMessage(chatID, processingMsg.MessageID, "😢 Sorry, I couldn't come up with dinner suggestions right now. Please try again later.")
					return
				}

				// Continue with just user suggestions
				aiSuggestions = []map[string]interface{}{}
			}

			// Combine AI and user suggestions
			if len(aiSuggestions) == 0 && len(userSuggestions) == 0 {
				bot.EditMessage(chatID, processingMsg.MessageID, "😢 I couldn't find any suitable dishes based on your fridge contents. Try adding more ingredients with /fridge or suggest your own dishes with /suggest.")
				return
			}

			// Calculate total number of suggestions
			totalSuggestions := len(aiSuggestions) + len(userSuggestions)

			// Create options for the poll
			options := make([]string, totalSuggestions)
			dishNames := make([]string, totalSuggestions)

			// Create a detailed message with suggestions
			detailedMsg := "🍲 Here are some dinner suggestions based on your ingredients:\n\n"

			// Add user suggestions first
			for i, suggestion := range userSuggestions {
				options[i] = suggestion.Name
				dishNames[i] = fmt.Sprintf("%s (%s) - suggested by @%s", suggestion.Name, suggestion.Cuisine, suggestion.Username)

				detailedMsg += fmt.Sprintf("🍴 *%s* (%s)\n%s\n_Suggested by @%s_\n\n", suggestion.Name, suggestion.Cuisine, suggestion.Description, suggestion.Username)

				// Mark the suggestion as used
				err := suggestService.MarkAsUsed(suggestion.ID)
				if err != nil {
					log.Error("Failed to mark suggestion as used: %v", err)
				}
			}

			// Add AI suggestions
			for i, suggestion := range aiSuggestions {
				name, _ := suggestion["name"].(string)
				cuisine, _ := suggestion["cuisine"].(string)
				description, _ := suggestion["description"].(string)

				// Add to options at the correct index (after user suggestions)
				index := len(userSuggestions) + i
				options[index] = name
				dishNames[index] = fmt.Sprintf("%s (%s)", name, cuisine)

				detailedMsg += fmt.Sprintf("🍴 *%s* (%s)\n%s\n\n", name, cuisine, description)
			}

			// Edit the processing message to show the detailed suggestions
			bot.EditMessage(chatID, processingMsg.MessageID, detailedMsg)

			// Create poll
			pollMsg, err := bot.CreatePoll(chatID, "What should we cook tonight?", options)
			if err != nil {
				log.Error("Failed to create poll: %v", err)
				errorMsg := messageService.GenerateErrorMessage("create poll")
				bot.SendMessage(chatID, errorMsg)
				return
			}

			// Store vote state
			_, err = pollService.CreateVote(chatID, fmt.Sprintf("%d", pollMsg.MessageID), pollMsg.MessageID, options)
			if err != nil {
				log.Error("Failed to create vote state: %v", err)
			}

			// Send a message with voting instructions
			bot.SendMessage(chatID, "🗳 Please vote for your preferred dinner option! The poll is above.")
		},
		"fridge": func(message *tgbotapi.Message) {
			// Show current ingredients
			chatID := message.Chat.ID

			ingredients, err := fridgeService.ListIngredients(chatID)
			if err != nil {
				log.Error("Failed to list ingredients: %v", err)
				bot.SendMessage(chatID, "😢 Sorry, I couldn't retrieve your fridge contents right now. Please try again later.")
				return
			}

			if len(ingredients) == 0 {
				bot.SendMessage(chatID, "Your fridge is empty! Add ingredients with /sync_fridge or by sending a photo with /add_photo.")
				return
			}

			// Create a formatted message with all ingredients
			msgText := "🧊 Here's what's in your fridge:\n\n"

			// Sort ingredients alphabetically
			sort.Slice(ingredients, func(i, j int) bool {
				return ingredients[i].Name < ingredients[j].Name
			})

			for _, ingredient := range ingredients {
				if ingredient.Quantity != "" {
					msgText += fmt.Sprintf("• %s (%s)\n", ingredient.Name, ingredient.Quantity)
				} else {
					msgText += fmt.Sprintf("• %s\n", ingredient.Name)
				}
			}

			bot.SendMessage(chatID, msgText)
		},
		"sync_fridge": func(message *tgbotapi.Message) {
			// Reset the fridge
			chatID := message.Chat.ID

			err := fridgeService.ResetFridge(chatID)
			if err != nil {
				log.Error("Failed to reset fridge: %v", err)
				errorMsg := messageService.GenerateErrorMessage("reset fridge")
				bot.SendMessage(chatID, errorMsg)
				return
			}

			// Set the chat state to adding ingredients
			stateManager.SetState(chatID, state.StateAddingIngredients)

			bot.SendMessage(chatID, "🧹 Fridge reset! Now, please send me a list of ingredients you have. You can send multiple messages, and I'll add all the ingredients to your fridge.")
		},
		"show_fridge": func(message *tgbotapi.Message) {
			// This is an alias for the /fridge command
			// Show current ingredients
			chatID := message.Chat.ID

			ingredients, err := fridgeService.ListIngredients(chatID)
			if err != nil {
				log.Error("Failed to list ingredients: %v", err)
				bot.SendMessage(chatID, "😢 Sorry, I couldn't retrieve your fridge contents right now. Please try again later.")
				return
			}

			if len(ingredients) == 0 {
				bot.SendMessage(chatID, "Your fridge is empty! Add ingredients with /sync_fridge or by sending a photo with /add_photo.")
				return
			}

			// Create a formatted message with all ingredients
			msgText := "🧊 Here's what's in your fridge:\n\n"

			// Sort ingredients alphabetically
			sort.Slice(ingredients, func(i, j int) bool {
				return ingredients[i].Name < ingredients[j].Name
			})

			for _, ingredient := range ingredients {
				if ingredient.Quantity != "" {
					msgText += fmt.Sprintf("• %s (%s)\n", ingredient.Name, ingredient.Quantity)
				} else {
					msgText += fmt.Sprintf("• %s\n", ingredient.Name)
				}
			}

			bot.SendMessage(chatID, msgText)
		},
		"add_photo": func(message *tgbotapi.Message) {
			chatID := message.Chat.ID

			// Set the chat state to adding photos
			stateManager.SetState(chatID, state.StateAddingPhotos)

			// If the message already has a photo, process it
			if message.Photo != nil && len(message.Photo) > 0 {
				// Get the largest photo (last in the array)
				photo := message.Photo[len(message.Photo)-1]

				// Send a processing message
				processingMsg, _ := bot.SendMessage(chatID, "🔍 Processing your photo... This might take a moment.")

				// Get the file URL
				photoURL, err := bot.GetFileURL(photo.FileID)
				if err != nil {
					log.Error("Failed to get photo URL: %v", err)
					bot.SendMessage(chatID, "😢 Sorry, I couldn't process your photo. Please try again.")
					return
				}

				// Extract ingredients from the photo
				ingredients, err := openaiClient.ExtractIngredientsFromPhoto(photoURL)
				if err != nil {
					log.Error("Failed to extract ingredients from photo: %v", err)
					bot.SendMessage(chatID, "😢 Sorry, I couldn't identify any ingredients in your photo. Please try again with a clearer photo.")
					return
				}

				if len(ingredients) == 0 {
					bot.SendMessage(chatID, "I couldn't identify any ingredients in your photo. Please try again with a clearer photo.")
					return
				}

				// Add ingredients to the fridge
				for _, ingredient := range ingredients {
					err := fridgeService.AddIngredient(chatID, ingredient, "")
					if err != nil {
						log.Error("Failed to add ingredient %s: %v", ingredient, err)
					}
				}

				// Edit the processing message to show the results
				bot.EditMessage(chatID, processingMsg.MessageID, fmt.Sprintf("✅ I found %d ingredients in your photo: %s", len(ingredients), strings.Join(ingredients, ", ")))

				// Ask if they want to add more photos
				keyboard := tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("Done adding photos", "done_adding_photos"),
					),
				)

				msg := tgbotapi.NewMessage(chatID, "Send more photos of your fridge or pantry, and I'll extract ingredients from them. Press 'Done' when you're finished.")
				msg.ReplyMarkup = keyboard
				bot.Send(msg)
			} else {
				// No photo in the command, instruct the user to send photos
				keyboard := tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("Cancel", "cancel_adding_photos"),
					),
				)

				msg := tgbotapi.NewMessage(chatID, "📷 Please send photos of your fridge or pantry, and I'll extract ingredients from them. Send as many photos as you need, and I'll process each one. Press 'Cancel' if you want to stop.")
				msg.ReplyMarkup = keyboard
				bot.Send(msg)
			}
		},
		"suggest": func(message *tgbotapi.Message) {
			// Start dish suggestion flow
			chatID := message.Chat.ID
			userID := fmt.Sprintf("%d", message.From.ID)
			username := message.From.UserName
			if username == "" {
				username = message.From.FirstName
			}

			// Check if there's a dish name in the command
			args := message.CommandArguments()
			if args != "" {
				// User provided a dish name with the command
				// Send a processing message
				processingMsg, _ := bot.SendMessage(chatID, fmt.Sprintf("🧐 Looking up information about '%s'... This might take a moment.", args))

				// Get dish information from OpenAI
				dishInfo, err := openaiClient.GetDishInfo(args)
				if err != nil {
					log.Error("Failed to get dish info: %v", err)
					bot.EditMessage(chatID, processingMsg.MessageID, fmt.Sprintf("😢 Sorry, I couldn't find information about '%s'. Please try again with a different dish.", args))
					return
				}

				// Extract dish information
				dishName, _ := dishInfo["name"].(string)
				if dishName == "" {
					dishName = args // Fallback to the user-provided name
				}

				cuisine, _ := dishInfo["cuisine"].(string)
				description, _ := dishInfo["description"].(string)

				// Get ingredients needed
				var ingredientsNeeded []string
				ingredientsList, ok := dishInfo["ingredients_needed"].([]interface{})
				if !ok {
					// Try alternative key
					ingredientsList, ok = dishInfo["ingredients"].([]interface{})
				}

				if ok {
					ingredientsNeeded = make([]string, len(ingredientsList))
					for i, ing := range ingredientsList {
						if ingStr, ok := ing.(string); ok {
							ingredientsNeeded[i] = ingStr
						}
					}
				}

				// Get fridge ingredients
				fridgeIngredients, err := fridgeService.ListIngredients(chatID)
				if err != nil {
					log.Error("Failed to list ingredients: %v", err)
					// Continue without fridge comparison
					fridgeIngredients = nil
				}

				// Compare ingredients
				var missingIngredients []string
				if len(fridgeIngredients) > 0 && len(ingredientsNeeded) > 0 {
					// Extract ingredient names from fridge
					fridgeNames := make([]string, len(fridgeIngredients))
					for i, ingredient := range fridgeIngredients {
						fridgeNames[i] = ingredient.Name
					}

					// Compare ingredients
					missingIngredients = dinner.CompareIngredients(ingredientsNeeded, fridgeNames)
				}

				// Add the suggestion
				suggestion, err := suggestService.AddSuggestion(chatID, userID, username, dishName, cuisine, description)
				if err != nil {
					log.Error("Failed to add suggestion: %v", err)
					bot.EditMessage(chatID, processingMsg.MessageID, fmt.Sprintf("😢 Sorry, I couldn't save your suggestion for '%s'. Please try again later.", args))
					return
				}

				// Create a detailed message about the dish
				detailedMsg := fmt.Sprintf("✅ Thanks for suggesting *%s* (%s cuisine)!\n\n%s\n\n", suggestion.Name, suggestion.Cuisine, suggestion.Description)

				// Add ingredients information
				if len(ingredientsNeeded) > 0 {
					detailedMsg += "*Ingredients needed:*\n"
					for _, ingredient := range ingredientsNeeded {
						detailedMsg += fmt.Sprintf("• %s\n", ingredient)
					}
					detailedMsg += "\n"
				}

				// Add missing ingredients information
				if len(missingIngredients) > 0 {
					detailedMsg += "*Missing from your fridge:*\n"
					for _, ingredient := range missingIngredients {
						detailedMsg += fmt.Sprintf("• %s\n", ingredient)
					}
					detailedMsg += "\n"
				}

				detailedMsg += "Your suggestion will be included in future dinner polls."

				// Edit the processing message with the detailed information
				bot.EditMessage(chatID, processingMsg.MessageID, detailedMsg)
			} else {
				// No dish name provided, ask for it
				bot.SendMessage(chatID, "🍴 You can suggest a dish for dinner! Please use the command like this: /suggest Lasagna")
			}
		},
		// TODO: Implement other command handlers
	}

	// Setup callback handlers
	callbackHandlers := map[string]telegram.CallbackHandler{
		// TODO: Implement callback handlers
	}

	// Setup default handler
	defaultHandler := func(update tgbotapi.Update) {
		// Skip if there's no message
		if update.Message == nil {
			return
		}

		chatID := update.Message.Chat.ID

		// Handle photos (without command)
		if len(update.Message.Photo) > 0 && !update.Message.IsCommand() {
			// Check if the chat is in adding ingredients state
			chatState := stateManager.GetState(chatID)
			if chatState == state.StateAddingIngredients || chatState == state.StateAddingPhotos {
				// Get the largest photo (last in the array)
				photo := update.Message.Photo[len(update.Message.Photo)-1]

				// Send a processing message
				processingMsg, _ := bot.SendMessage(chatID, "🔍 Processing your photo... This might take a moment.")

				// Get the file URL
				photoURL, err := bot.GetFileURL(photo.FileID)
				if err != nil {
					log.Error("Failed to get photo URL: %v", err)
					bot.SendMessage(chatID, "😢 Sorry, I couldn't process your photo. Please try again.")
					return
				}

				// Extract ingredients from the photo
				ingredients, err := openaiClient.ExtractIngredientsFromPhoto(photoURL)
				if err != nil {
					log.Error("Failed to extract ingredients from photo: %v", err)
					bot.SendMessage(chatID, "😢 Sorry, I couldn't identify any ingredients in your photo. Please try again with a clearer photo.")
					return
				}

				if len(ingredients) == 0 {
					bot.SendMessage(chatID, "I couldn't identify any ingredients in your photo. Please try again with a clearer photo.")
					return
				}

				// Add ingredients to the fridge
				for _, ingredient := range ingredients {
					err := fridgeService.AddIngredient(chatID, ingredient, "")
					if err != nil {
						log.Error("Failed to add ingredient %s: %v", ingredient, err)
					}
				}

				// Edit the processing message to show the results
				bot.EditMessage(chatID, processingMsg.MessageID, fmt.Sprintf("✅ I found %d ingredients in your photo: %s", len(ingredients), strings.Join(ingredients, ", ")))

				// Different buttons based on the state
				var keyboard tgbotapi.InlineKeyboardMarkup
				var promptText string

				if chatState == state.StateAddingIngredients {
					// For text-based ingredient adding
					keyboard = tgbotapi.NewInlineKeyboardMarkup(
						tgbotapi.NewInlineKeyboardRow(
							tgbotapi.NewInlineKeyboardButtonData("Done adding ingredients", "done_adding"),
							tgbotapi.NewInlineKeyboardButtonData("Add more", "add_more"),
						),
					)
					promptText = "Would you like to add more ingredients or are you done?"
				} else {
					// For photo-based ingredient adding
					keyboard = tgbotapi.NewInlineKeyboardMarkup(
						tgbotapi.NewInlineKeyboardRow(
							tgbotapi.NewInlineKeyboardButtonData("Done adding photos", "done_adding_photos"),
						),
					)
					promptText = "Send more photos of your fridge or pantry, and I'll extract ingredients from them. Press 'Done' when you're finished."
				}

				msg := tgbotapi.NewMessage(chatID, promptText)
				msg.ReplyMarkup = keyboard
				bot.Send(msg)
			} else {
				// Suggest using /add_photo command
				bot.SendMessage(chatID, "I see you sent a photo! If you want me to extract ingredients from it, please use the /add_photo command.")
			}
			return
		}

		// Handle text messages
		if update.Message.Text != "" && !update.Message.IsCommand() {
			text := update.Message.Text

			// Check if the chat is in adding ingredients state
			if stateManager.GetState(chatID) == state.StateAddingIngredients {
				// Parse ingredients from the text
				ingredients, err := openaiClient.ParseIngredientsFromText(text)
				if err != nil {
					log.Error("Failed to parse ingredients: %v", err)
					bot.SendMessage(chatID, fmt.Sprintf("😢 Sorry, I couldn't understand the ingredients. Please try again with a clearer list."))
					return
				}

				if len(ingredients) == 0 {
					bot.SendMessage(chatID, "I couldn't find any ingredients in your message. Please try again with a list of ingredients.")
					return
				}

				// Add ingredients to the fridge
				for _, ingredient := range ingredients {
					err := fridgeService.AddIngredient(chatID, ingredient, "")
					if err != nil {
						log.Error("Failed to add ingredient %s: %v", ingredient, err)
					}
				}

				// Confirm the ingredients were added
				bot.SendMessage(chatID, fmt.Sprintf("✅ Added %d ingredients to your fridge: %s", len(ingredients), strings.Join(ingredients, ", ")))

				// Ask if they want to add more
				keyboard := tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("Done adding ingredients", "done_adding"),
						tgbotapi.NewInlineKeyboardButtonData("Add more", "add_more"),
					),
				)

				msg := tgbotapi.NewMessage(chatID, "Would you like to add more ingredients or are you done?")
				msg.ReplyMarkup = keyboard
				bot.Send(msg)
			} else if stateManager.GetState(chatID) == state.StateSuggestingDish {
				// We're now handling this directly in the /suggest command
				// Just clear the state and ask the user to use the command
				stateManager.ClearState(chatID)
				bot.SendMessage(chatID, "🍴 Please use the /suggest command followed by a dish name, like: /suggest Lasagna")
			} else {
				// Regular ingredient adding (single ingredient)
				// Check if it looks like an ingredient
				if !strings.Contains(text, " ") && len(text) < 30 {
					err := fridgeService.AddIngredient(chatID, text, "")
					if err != nil {
						log.Error("Failed to add ingredient: %v", err)
						bot.SendMessage(chatID, fmt.Sprintf("😢 Sorry, I couldn't add %s to your fridge.", text))
						return
					}

					bot.SendMessage(chatID, fmt.Sprintf("✅ Added %s to your fridge!", text))
				}
			}
		}
	}

	// Add callback handler for ingredient adding buttons
	callbackHandlers["done_adding"] = func(callback *tgbotapi.CallbackQuery) {
		chatID := callback.Message.Chat.ID

		// Clear the state
		stateManager.ClearState(chatID)

		// Answer the callback
		bot.AnswerCallbackQuery(callback.ID, "Thanks! Your fridge is now updated.")

		// Edit the message to remove the buttons
		editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "✅ Fridge update complete! Use /fridge to see your ingredients or /dinner to get dinner suggestions.")
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}
		bot.Send(editMsg)
	}

	callbackHandlers["add_more"] = func(callback *tgbotapi.CallbackQuery) {
		chatID := callback.Message.Chat.ID

		// Keep the state as is

		// Answer the callback
		bot.AnswerCallbackQuery(callback.ID, "Please send more ingredients!")

		// Edit the message to remove the buttons
		editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "Please send more ingredients. I'll add them to your fridge.")
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}
		bot.Send(editMsg)
	}

	callbackHandlers["show_fridge"] = func(callback *tgbotapi.CallbackQuery) {
		chatID := callback.Message.Chat.ID

		// Answer the callback
		bot.AnswerCallbackQuery(callback.ID, "Here's what's in your fridge!")

		// Edit the message to remove the buttons
		editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "Here's what's in your fridge:")
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}
		bot.Send(editMsg)

		// Show fridge contents
		ingredients, err := fridgeService.ListIngredients(chatID)
		if err != nil {
			log.Error("Failed to list ingredients: %v", err)
			bot.SendMessage(chatID, "😢 Sorry, I couldn't retrieve your fridge contents right now. Please try again later.")
			return
		}

		if len(ingredients) == 0 {
			bot.SendMessage(chatID, "Your fridge is empty! Add ingredients with /sync_fridge or by sending a photo with /add_photo.")
			return
		}

		// Create a formatted message with all ingredients
		msgText := "🧊 Here's what's in your fridge:\n\n"

		// Sort ingredients alphabetically
		sort.Slice(ingredients, func(i, j int) bool {
			return ingredients[i].Name < ingredients[j].Name
		})

		for _, ingredient := range ingredients {
			if ingredient.Quantity != "" {
				msgText += fmt.Sprintf("• %s (%s)\n", ingredient.Name, ingredient.Quantity)
			} else {
				msgText += fmt.Sprintf("• %s\n", ingredient.Name)
			}
		}

		bot.SendMessage(chatID, msgText)
	}

	callbackHandlers["done_adding_photos"] = func(callback *tgbotapi.CallbackQuery) {
		chatID := callback.Message.Chat.ID

		// Clear the state
		stateManager.ClearState(chatID)

		// Answer the callback
		bot.AnswerCallbackQuery(callback.ID, "Thanks! Your fridge is now updated with ingredients from your photos.")

		// Edit the message to remove the buttons
		editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "✅ Photo processing complete! I've added all the ingredients I found to your fridge.")
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}
		bot.Send(editMsg)

		// Show fridge contents
		ingredients, err := fridgeService.ListIngredients(chatID)
		if err != nil {
			log.Error("Failed to list ingredients: %v", err)
			return
		}

		if len(ingredients) == 0 {
			bot.SendMessage(chatID, "Your fridge is still empty. Try adding ingredients with text or better photos.")
			return
		}

		// Create a formatted message with all ingredients
		msgText := "🧊 Here's what's in your fridge:\n\n"

		// Sort ingredients alphabetically
		sort.Slice(ingredients, func(i, j int) bool {
			return ingredients[i].Name < ingredients[j].Name
		})

		for _, ingredient := range ingredients {
			if ingredient.Quantity != "" {
				msgText += fmt.Sprintf("• %s (%s)\n", ingredient.Name, ingredient.Quantity)
			} else {
				msgText += fmt.Sprintf("• %s\n", ingredient.Name)
			}
		}

		bot.SendMessage(chatID, msgText)

		// Suggest next steps
		bot.SendMessage(chatID, "You can now use /dinner to get dinner suggestions based on your ingredients!")
	}

	callbackHandlers["cancel_adding_photos"] = func(callback *tgbotapi.CallbackQuery) {
		chatID := callback.Message.Chat.ID

		// Clear the state
		stateManager.ClearState(chatID)

		// Answer the callback
		bot.AnswerCallbackQuery(callback.ID, "Photo adding cancelled.")

		// Edit the message to remove the buttons
		editMsg := tgbotapi.NewEditMessageText(chatID, callback.Message.MessageID, "Photo adding cancelled. You can use /fridge to see your current ingredients or /dinner to get dinner suggestions.")
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}
		bot.Send(editMsg)
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Info("Shutting down...")
		store.Close()
		os.Exit(0)
	}()

	// Start the bot
	log.Info("Bot is now running. Press CTRL-C to exit.")
	if err := bot.Start(commandHandlers, callbackHandlers, defaultHandler); err != nil {
		log.Error("Error running bot: %v", err)
		os.Exit(1)
	}
}

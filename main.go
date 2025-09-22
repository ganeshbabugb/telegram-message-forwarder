package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/zelenin/go-tdlib/client"
	"golang.org/x/sync/singleflight"
)

// Config represents the structure of the data in config.json.
type Config struct {
	SourceChannels map[string][]string `json:"sourceChannels"`
	ForwardTo      []string            `json:"forwardTo"`
}

// SourceChannelInfo holds details for a Telegram source channel.
type SourceChannelInfo struct {
	ChatID      int64
	ChannelName string
	Keywords    []string
}

// ForwardDestinationInfo holds details for a Telegram destination channel.
type ForwardDestinationInfo struct {
	ChatID      int64
	ChannelName string
}

// Upgrader for WebSocket connections.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var (
	websocketClients = make(map[*websocket.Conn]struct{})
	clientsMutex     sync.Mutex // Mutex to protect the clients map
	broadcastChannel = make(chan string)
	mostRecentQRLink string
	logger           *slog.Logger
)

func init() {
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
	}))
}

func handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("Failed to upgrade WebSocket connection", "error", err)
		return
	}

	clientsMutex.Lock()
	websocketClients[conn] = struct{}{}
	clientsMutex.Unlock()
	logger.Info("New WebSocket client connected", "total_clients", len(websocketClients))

	if mostRecentQRLink != "" {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(mostRecentQRLink)); err != nil {
			logger.Error("Failed to send initial QR link to client", "error", err)
		}
	}

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			clientsMutex.Lock()
			delete(websocketClients, conn)
			clientsMutex.Unlock()

			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.Info("WebSocket client disconnected gracefully")
			} else {
				logger.Error("WebSocket client disconnected with an error", "error", err)
			}
			conn.Close()
			break
		}
	}
}

func handleBroadcast() {
	for qrLink := range broadcastChannel {
		mostRecentQRLink = qrLink
		clientsMutex.Lock()
		for clientConn := range websocketClients {
			if err := clientConn.WriteMessage(websocket.TextMessage, []byte(qrLink)); err != nil {
				logger.Error("Failed to broadcast QR link to client", "error", err)
				clientConn.Close()
				delete(websocketClients, clientConn)
			}
		}
		clientsMutex.Unlock()
	}
}

func main() {
	authMethod := flag.String("auth", "qr", "Authentication method: 'cli' or 'qr'")
	flag.Parse()

	config, err := loadConfig("config.json")
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("Configuration loaded successfully")

	apiIDStr := os.Getenv("API_ID")
	apiHash := os.Getenv("API_HASH")

	if apiIDStr == "" || apiHash == "" {
		logger.Error("API_ID and API_HASH environment variables must be set")
		os.Exit(1)
	}

	apiID64, err := strconv.ParseInt(apiIDStr, 10, 32)
	if err != nil {
		logger.Error("Failed to parse API_ID", "error", err)
		os.Exit(1)
	}
	apiID := int32(apiID64)

	tdlibParameters := &client.SetTdlibParametersRequest{
		UseTestDc:           false,
		DatabaseDirectory:   filepath.Join(".tdlib", "database"),
		FilesDirectory:      filepath.Join(".tdlib", "files"),
		UseFileDatabase:     true,
		UseChatInfoDatabase: true,
		UseMessageDatabase:  true,
		UseSecretChats:      false,
		ApiId:               apiID,
		ApiHash:             apiHash,
		SystemLanguageCode:  "en",
		DeviceModel:         "Server",
		SystemVersion:       "1.0.0",
		ApplicationVersion:  "1.0.0",
	}

	var authorizer client.AuthorizationStateHandler

	updatesChannel := make(chan *client.Message, 100)

	switch *authMethod {
	case "cli":
		logger.Info("Using CLI authentication. Follow the prompts.")
		cliAuthorizer := client.ClientAuthorizer(tdlibParameters)
		go client.CliInteractor(cliAuthorizer)
		authorizer = cliAuthorizer
	case "qr":
		authorizer = client.QrAuthorizer(tdlibParameters, func(link string) error {
			logger.Info("New QR code link generated", "link", link)
			broadcastChannel <- link
			return nil
		})
	default:
		logger.Error("Invalid authentication method. Use 'cli' or 'qr'.", "method", *authMethod)
		os.Exit(1)
	}

	if _, err := client.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{
		NewVerbosityLevel: 1,
	}); err != nil {
		logger.Error("Failed to set TDLib log verbosity", "error", err)
	}

	go func() {
		http.HandleFunc("/ws", handleWebSocketConnection)
		http.Handle("/", http.FileServer(http.Dir("./static")))
		logger.Info("Web server started", "port", 8080)
		if err := http.ListenAndServe(":8080", nil); err != nil {
			logger.Error("Web server failed", "error", err)
		}
	}()

	go handleBroadcast()

	tdlibClient, err := client.NewClient(authorizer, client.WithResultHandler(client.NewCallbackResultHandler(
		func(result client.Type) {
			// logger.Info("result.GetType(): ", "Update Type", result.GetType())
			if update, ok := result.(*client.UpdateNewMessage); ok {
				updatesChannel <- update.Message
			}
		},
	)))
	if err != nil {
		logger.Error("Failed to create TDLib client", "error", err)
		os.Exit(1)
	}

	user, err := tdlibClient.GetMe(context.Background())
	if err != nil {
		logger.Error("Failed to get user profile details", "error", err)
		os.Exit(1)
	}
	logger.Info("TDLib client authenticated", "user_first_name", user.FirstName, "user_phone_number", user.PhoneNumber)

	sourceChannels, forwardDestinations, err := findChats(tdlibClient, config)
	if err != nil {
		logger.Error("Failed to find and map chats", "error", err)
		os.Exit(1)
	}

	chatKeywords := make(map[int64][]string, len(sourceChannels))
	for i := range sourceChannels {
		sc := &sourceChannels[i]
		chatKeywords[sc.ChatID] = sc.Keywords
	}

	var group singleflight.Group

	shutdownChan := make(chan os.Signal, 2)
	signal.Notify(shutdownChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case msg := <-updatesChannel:
			if msg.IsOutgoing {
				continue
			}

			keywords, ok := chatKeywords[msg.ChatId]
			if !ok {
				continue
			}

			var messageText string
			switch content := msg.Content.(type) {
			case *client.MessageText:
				messageText = content.Text.Text
			case *client.MessagePhoto:
				messageText = content.Caption.Text
			default:
				continue
			}

			messageText = strings.ToLower(messageText)

			if containsKeywords(messageText, keywords) {
				logger.Info("Keyword found in a message", "from_chat_id", msg.ChatId, "message_id", msg.Id, "keywords", keywords)
				messageKey := strconv.FormatInt(msg.Id, 10)
				group.Do(messageKey, func() (interface{}, error) {
					for _, destination := range forwardDestinations {

						if msg.ChatId == destination.ChatID {
							logger.Info("Skipping message forward to the same source channel", "channel_name", destination.ChannelName)
							continue
						}

						logger.Info("Forwarding message", "source_chat_id", msg.ChatId, "destination_chat_id", destination.ChatID, "destination_channel", destination.ChannelName)
						if _, err := tdlibClient.ForwardMessages(context.Background(),
							&client.ForwardMessagesRequest{
								ChatId:     destination.ChatID,
								FromChatId: msg.ChatId,
								MessageIds: []int64{msg.Id},
								Options:    &client.MessageSendOptions{},
							}); err != nil {
							logger.Error("Failed to forward message", "destination_channel", destination.ChannelName, "error", err)
						}
					}
					return nil, nil
				})
			}
		case <-shutdownChan:
			logger.Info("Initiating graceful shutdown...")
			tdlibClient.Close(context.Background())
			return
		}
	}
}

func containsKeywords(text string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func loadConfig(path string) (*Config, error) {
	configEnv := os.Getenv("CONFIG_JSON")
	logger.Info("CONFIG:", "CONFIG_JSON: ", configEnv)
	if configEnv != "" {
		var cfg Config
		if err := json.Unmarshal([]byte(configEnv), &cfg); err != nil {
			return nil, fmt.Errorf("invalid CONFIG_JSON: %w", err)
		}

		// Write back to config.json for compatibility
		data, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(path, data, 0644)

		return &cfg, nil
	}

	// Fallback → load from config.json file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &cfg, nil
}

func findChats(tdlibClient *client.Client, config *Config) ([]SourceChannelInfo, []ForwardDestinationInfo, error) {
	chats, err := tdlibClient.GetChats(context.Background(), &client.GetChatsRequest{Limit: 1000})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get chats: %w", err)
	}
	logger.Info("Fetched chats successfully")

	forwardDestinationsMap := make(map[string]struct{}, len(config.ForwardTo))
	for _, name := range config.ForwardTo {
		forwardDestinationsMap[name] = struct{}{}
	}

	var sourceChannels []SourceChannelInfo
	var forwardDestinations []ForwardDestinationInfo

	for _, chatID := range chats.ChatIds {
		chat, err := tdlibClient.GetChat(context.Background(), &client.GetChatRequest{ChatId: chatID})
		if err != nil {
			logger.Warn("Failed to get chat details", "chat_id", chatID, "error", err)
			continue
		}

		if keywords, ok := config.SourceChannels[chat.Title]; ok {
			loweredKeywords := make([]string, len(keywords))
			for i, kw := range keywords {
				loweredKeywords[i] = strings.ToLower(kw)
			}
			sourceChannels = append(sourceChannels, SourceChannelInfo{
				ChatID:      chat.Id,
				ChannelName: chat.Title,
				Keywords:    loweredKeywords,
			})
			logger.Info("Identified source channel", "channel_name", chat.Title)
		}

		if _, ok := forwardDestinationsMap[chat.Title]; ok {
			forwardDestinations = append(forwardDestinations, ForwardDestinationInfo{
				ChatID:      chat.Id,
				ChannelName: chat.Title,
			})
			logger.Info("Identified forward destination", "channel_name", chat.Title)
		}
	}
	return sourceChannels, forwardDestinations, nil
}

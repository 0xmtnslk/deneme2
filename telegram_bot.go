package main

import (
        "crypto/aes"
        "crypto/cipher"
        "crypto/rand"
        "crypto/sha256"
        "encoding/base64"
        json "github.com/json-iterator/go"
        "fmt"
        "io"
        "io/ioutil"
        "log"
        "os"
        "strconv"
        "strings"
        "sync"
        "time"

        "github.com/fsnotify/fsnotify"
        tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// UserState represents user's current setup state
type UserState string

const (
        StateNone             UserState = "none"
        StateConfirmAPIChange UserState = "confirm_api_change"
        StateAwaitingKey      UserState = "awaiting_api_key"
        StateAwaitingSecret   UserState = "awaiting_secret"
        StateAwaitingPasskey  UserState = "awaiting_passkey"  
        StateAwaitingMargin   UserState = "awaiting_margin"
        StateAwaitingLeverage UserState = "awaiting_leverage"
        StateComplete         UserState = "complete"
)

// UserData represents individual user settings and API credentials
type UserData struct {
        UserID        int64     `json:"user_id"`
        Username      string    `json:"username"`
        BitgetAPIKey  string    `json:"bitget_api_key"`      // Encrypted when stored
        BitgetSecret  string    `json:"bitget_secret"`       // Encrypted when stored
        BitgetPasskey string    `json:"bitget_passkey"`      // Encrypted when stored
        MarginUSDT    float64   `json:"margin_usdt"`
        Leverage      int       `json:"leverage"`
        IsActive      bool      `json:"is_active"`
        State         UserState `json:"current_state"`
        CreatedAt     string    `json:"created_at"`
        UpdatedAt     string    `json:"updated_at"`
}

// PositionInfo stores position tracking data for reminders
type PositionInfo struct {
        UserID      int64   `json:"user_id"`
        Symbol      string  `json:"symbol"`
        OrderID     string  `json:"order_id"`
        OpenPrice   float64 `json:"open_price"`
        Size        float64 `json:"size"`
        MarginUSDT  float64 `json:"margin_usdt"`
        Leverage    int     `json:"leverage"`
        OpenTime    time.Time `json:"open_time"`
        LastReminder time.Time `json:"last_reminder"`
}

// ActivePositions stores currently tracked positions with thread-safe access
var (
        activePositions = make(map[string]*PositionInfo)
        positionsMutex  sync.RWMutex
)

const positionsFile = "active_positions.json"

// BotDatabase represents multi-user storage
type BotDatabase struct {
        Users map[int64]*UserData `json:"users"`
        mutex sync.RWMutex
}

// TelegramBot represents our multi-user bot
type TelegramBot struct {
        bot          *tgbotapi.BotAPI
        database     *BotDatabase
        dbFile       string
        encryptionKey []byte
        lastProcessedSymbol string // Track last processed coin to prevent duplicates
        upbitMonitor *UpbitMonitor // Reference to monitor for trade logging
}

// Generate encryption key from environment (required for persistence)
func generateEncryptionKey() ([]byte, error) {
        envKey := os.Getenv("BOT_ENCRYPTION_KEY")
        if envKey == "" {
                return nil, fmt.Errorf("BOT_ENCRYPTION_KEY environment variable is required for secure credential storage")
        }
        
        // First try to decode as base64 (proper format)
        if key, err := base64.StdEncoding.DecodeString(envKey); err == nil && len(key) == 32 {
                return key, nil
        }
        
        // Fallback: hash the string to create consistent 32-byte key
        // This supports legacy string-based keys
        hash := sha256.Sum256([]byte(envKey))
        return hash[:], nil
}

// Encrypt sensitive data using AES-GCM
func (tb *TelegramBot) encryptSensitiveData(plaintext string) (string, error) {
        if plaintext == "" {
                return "", nil
        }
        
        block, err := aes.NewCipher(tb.encryptionKey)
        if err != nil {
                return "", fmt.Errorf("failed to create cipher: %v", err)
        }
        
        gcm, err := cipher.NewGCM(block)
        if err != nil {
                return "", fmt.Errorf("failed to create GCM: %v", err)
        }
        
        nonce := make([]byte, gcm.NonceSize())
        if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
                return "", fmt.Errorf("failed to generate nonce: %v", err)
        }
        
        ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
        return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt sensitive data using AES-GCM
func (tb *TelegramBot) decryptSensitiveData(ciphertext string) (string, error) {
        if ciphertext == "" {
                return "", nil
        }
        
        data, err := base64.StdEncoding.DecodeString(ciphertext)
        if err != nil {
                return "", fmt.Errorf("failed to decode base64: %v", err)
        }
        
        block, err := aes.NewCipher(tb.encryptionKey)
        if err != nil {
                return "", fmt.Errorf("failed to create cipher: %v", err)
        }
        
        gcm, err := cipher.NewGCM(block)
        if err != nil {
                return "", fmt.Errorf("failed to create GCM: %v", err)
        }
        
        nonceSize := gcm.NonceSize()
        if len(data) < nonceSize {
                return "", fmt.Errorf("ciphertext too short")
        }
        
        nonce, ciphertext_bytes := data[:nonceSize], data[nonceSize:]
        plaintext, err := gcm.Open(nil, nonce, ciphertext_bytes, nil)
        if err != nil {
                return "", fmt.Errorf("failed to decrypt: %v", err)
        }
        
        return string(plaintext), nil
}

// NewTelegramBot creates a new bot instance with encryption
func NewTelegramBot(token string) (*TelegramBot, error) {
        bot, err := tgbotapi.NewBotAPI(token)
        if err != nil {
                return nil, fmt.Errorf("failed to create bot: %v", err)
        }

        // Generate or get encryption key
        encryptionKey, err := generateEncryptionKey()
        if err != nil {
                return nil, fmt.Errorf("failed to setup encryption: %v", err)
        }

        botInstance := &TelegramBot{
                bot:           bot,
                dbFile:        "bot_users.json",
                encryptionKey: encryptionKey,
                database: &BotDatabase{
                        Users: make(map[int64]*UserData),
                },
        }

        // Load existing user data (will decrypt automatically)
        if err := botInstance.loadDatabase(); err != nil {
                log.Printf("Warning: Could not load database: %v", err)
        }

        // Load saved positions from previous sessions
        loadActivePositions()
        
        // Start file watcher for upbit_new.json
        go botInstance.startFileWatcher()
        
        // Start position reminder system
        go botInstance.startPositionReminders()

        // Start 4-hour status notifications
        go botInstance.startStatusNotifications()

        return botInstance, nil
}

// Save user database to JSON file (assumes caller has mutex lock)
func (tb *TelegramBot) saveDatabaseUnsafe() error {
        data, err := json.MarshalIndent(tb.database, "", "  ")
        if err != nil {
                return fmt.Errorf("failed to marshal database: %v", err)
        }

        return ioutil.WriteFile(tb.dbFile, data, 0644)
}

// Save user database to JSON file (thread-safe)
func (tb *TelegramBot) saveDatabase() error {
        tb.database.mutex.Lock()
        defer tb.database.mutex.Unlock()
        return tb.saveDatabaseUnsafe()
}

// Load user database from JSON file
func (tb *TelegramBot) loadDatabase() error {
        if _, err := os.Stat(tb.dbFile); os.IsNotExist(err) {
                return nil // File doesn't exist yet
        }

        data, err := ioutil.ReadFile(tb.dbFile)
        if err != nil {
                return fmt.Errorf("failed to read database file: %v", err)
        }

        tb.database.mutex.Lock()
        defer tb.database.mutex.Unlock()

        return json.Unmarshal(data, tb.database)
}

// Get user data by ID (decrypts sensitive fields)
func (tb *TelegramBot) getUser(userID int64) (*UserData, bool) {
        tb.database.mutex.RLock()
        defer tb.database.mutex.RUnlock()
        
        encryptedUser, exists := tb.database.Users[userID]
        if !exists {
                return nil, false
        }
        
        // Create a copy for decryption
        user := *encryptedUser
        
        // Decrypt sensitive fields
        if encryptedUser.BitgetAPIKey != "" {
                decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetAPIKey)
                if err != nil {
                        log.Printf("Warning: Failed to decrypt API key for user %d: %v", userID, err)
                        // Return user with empty credentials rather than failing completely
                        user.BitgetAPIKey = ""
                } else {
                        user.BitgetAPIKey = decrypted
                }
        }
        
        if encryptedUser.BitgetSecret != "" {
                decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetSecret)
                if err != nil {
                        log.Printf("Warning: Failed to decrypt secret for user %d: %v", userID, err)
                        user.BitgetSecret = ""
                } else {
                        user.BitgetSecret = decrypted
                }
        }
        
        if encryptedUser.BitgetPasskey != "" {
                decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetPasskey)
                if err != nil {
                        log.Printf("Warning: Failed to decrypt passkey for user %d: %v", userID, err)
                        user.BitgetPasskey = ""
                } else {
                        user.BitgetPasskey = decrypted
                }
        }
        
        return &user, true
}

// Save or update user data (encrypts sensitive fields before saving)
func (tb *TelegramBot) saveUser(user *UserData) error {
        tb.database.mutex.Lock()
        defer tb.database.mutex.Unlock()

        user.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
        if user.CreatedAt == "" {
                user.CreatedAt = user.UpdatedAt
        }

        // Create a copy for encryption (don't modify original)
        encryptedUser := *user
        
        // Encrypt sensitive fields before saving
        if user.BitgetAPIKey != "" {
                encrypted, err := tb.encryptSensitiveData(user.BitgetAPIKey)
                if err != nil {
                        return fmt.Errorf("failed to encrypt API key: %v", err)
                }
                encryptedUser.BitgetAPIKey = encrypted
        }
        
        if user.BitgetSecret != "" {
                encrypted, err := tb.encryptSensitiveData(user.BitgetSecret)
                if err != nil {
                        return fmt.Errorf("failed to encrypt secret: %v", err)
                }
                encryptedUser.BitgetSecret = encrypted
        }
        
        if user.BitgetPasskey != "" {
                encrypted, err := tb.encryptSensitiveData(user.BitgetPasskey)
                if err != nil {
                        return fmt.Errorf("failed to encrypt passkey: %v", err)
                }
                encryptedUser.BitgetPasskey = encrypted
        }

        tb.database.Users[user.UserID] = &encryptedUser
        return tb.saveDatabaseUnsafe() // Use unsafe version since we already have lock
}

// Get all active users (with decrypted credentials)
func (tb *TelegramBot) getAllActiveUsers() []*UserData {
        tb.database.mutex.RLock()
        defer tb.database.mutex.RUnlock()

        var activeUsers []*UserData
        for _, encryptedUser := range tb.database.Users {
                if encryptedUser.IsActive {
                        // Decrypt sensitive fields for each user
                        user := *encryptedUser
                        
                        if encryptedUser.BitgetAPIKey != "" {
                                if decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetAPIKey); err == nil {
                                        user.BitgetAPIKey = decrypted
                                } else {
                                        log.Printf("Warning: Failed to decrypt API key for user %d: %v", encryptedUser.UserID, err)
                                        user.BitgetAPIKey = ""
                                }
                        }
                        
                        if encryptedUser.BitgetSecret != "" {
                                if decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetSecret); err == nil {
                                        user.BitgetSecret = decrypted
                                } else {
                                        log.Printf("Warning: Failed to decrypt secret for user %d: %v", encryptedUser.UserID, err)
                                        user.BitgetSecret = ""
                                }
                        }
                        
                        if encryptedUser.BitgetPasskey != "" {
                                if decrypted, err := tb.decryptSensitiveData(encryptedUser.BitgetPasskey); err == nil {
                                        user.BitgetPasskey = decrypted
                                } else {
                                        log.Printf("Warning: Failed to decrypt passkey for user %d: %v", encryptedUser.UserID, err)
                                        user.BitgetPasskey = ""
                                }
                        }
                        
                        activeUsers = append(activeUsers, &user)
                }
        }
        return activeUsers
}

// Start file watcher for upbit_new.json to trigger auto-trading
func (tb *TelegramBot) startFileWatcher() {
        log.Printf("🔧 Starting file watcher...")
        
        watcher, err := fsnotify.NewWatcher()
        if err != nil {
                log.Printf("❌ Failed to create file watcher: %v", err)
                return
        }
        defer watcher.Close()

        // Watch upbit_new.json file - use absolute path for reliability
        upbitFile := "upbit_new.json"
        
        // Check if file exists first
        if _, err := os.Stat(upbitFile); os.IsNotExist(err) {
                log.Printf("❌ File %s does not exist!", upbitFile)
                return
        }
        
        err = watcher.Add(upbitFile)
        if err != nil {
                log.Printf("❌ Failed to watch %s: %v", upbitFile, err)
                return
        }

        log.Printf("👁️  Successfully watching %s for new UPBIT listings...", upbitFile)

        // Initialize with current latest symbol to prevent triggering on startup
        if latestSymbol := tb.getLatestDetectedSymbol(); latestSymbol != "" {
                tb.lastProcessedSymbol = latestSymbol
                log.Printf("🔄 Current latest symbol: %s", latestSymbol)
        }

        log.Printf("🔄 File watcher ready - waiting for events...")
        
        for {
                select {
                case event, ok := <-watcher.Events:
                        if !ok {
                                log.Printf("❌ File watcher events channel closed")
                                return
                        }
                        log.Printf("📝 File event detected: %s (Op: %v)", event.Name, event.Op)
                        if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Chmod == fsnotify.Chmod {
                                log.Printf("🚨 FILE CHANGE EVENT - Processing file change: %s", event.Name)
                                tb.processUpbitFile()
                        } else {
                                log.Printf("📋 Event ignored: %v", event.Op)
                        }
                case err, ok := <-watcher.Errors:
                        if !ok {
                                log.Printf("❌ File watcher error channel closed")
                                return
                        }
                        log.Printf("❌ File watcher error: %v", err)
                }
        }
}

// Get latest detected symbol from upbit_new.json
func (tb *TelegramBot) getLatestDetectedSymbol() string {
        data, err := ioutil.ReadFile("upbit_new.json")
        if err != nil {
                log.Printf("Warning: Could not read upbit_new.json: %v", err)
                return ""
        }

        // Parse JSONL format - read last line for most recent entry
        lines := strings.Split(strings.TrimSpace(string(data)), "\n")
        if len(lines) == 0 {
                return ""
        }

        // Get the last non-empty line (most recent entry)
        var lastEntry ListingEntry
        for i := len(lines) - 1; i >= 0; i-- {
                line := strings.TrimSpace(lines[i])
                if line == "" {
                        continue
                }
                
                if err := json.Unmarshal([]byte(line), &lastEntry); err != nil {
                        log.Printf("Warning: Could not parse JSON line: %v", err)
                        continue
                }
                
                return lastEntry.Symbol
        }

        return ""
}

// Process upbit_new.json changes and trigger auto-trading
func (tb *TelegramBot) processUpbitFile() {
        latestSymbol := tb.getLatestDetectedSymbol()
        if latestSymbol == "" {
                return
        }

        // Check if this is a new symbol we haven't processed yet
        if latestSymbol == tb.lastProcessedSymbol {
                log.Printf("🔄 Symbol %s already processed, skipping", latestSymbol)
                return
        }

        // Update last processed symbol
        tb.lastProcessedSymbol = latestSymbol
        log.Printf("🚨 NEW UPBIT LISTING DETECTED: %s", latestSymbol)

        // Get all active users for auto-trading
        activeUsers := tb.getAllActiveUsers()
        if len(activeUsers) == 0 {
                log.Printf("⚠️  No active users found for auto-trading")
                return
        }

        log.Printf("📊 Triggering auto-trading for %d users on symbol: %s", len(activeUsers), latestSymbol)

        // Trigger auto-trading for each active user
        for _, user := range activeUsers {
                go tb.executeAutoTrade(user, latestSymbol)
        }
}

// Execute automatic trading for a user when new UPBIT listing is detected
func (tb *TelegramBot) executeAutoTrade(user *UserData, symbol string) {
        log.Printf("🤖 Auto-trading for user %d (%s) on symbol: %s", user.UserID, user.Username, symbol)

        // Validate user has complete setup
        if user.BitgetAPIKey == "" || user.BitgetSecret == "" || user.BitgetPasskey == "" {
                log.Printf("⚠️  User %d missing API credentials, skipping auto-trade", user.UserID)
                tb.sendMessage(user.UserID, fmt.Sprintf("🚫 Auto-trade failed for %s: Missing API credentials. Please /setup first.", symbol))
                return
        }

        if user.MarginUSDT <= 0 {
                log.Printf("⚠️  User %d has invalid margin amount: %f", user.UserID, user.MarginUSDT)
                tb.sendMessage(user.UserID, fmt.Sprintf("🚫 Auto-trade failed for %s: Invalid margin amount. Please /setup first.", symbol))
                return
        }

        // Format symbol for Bitget (add USDT suffix)
        tradingSymbol := symbol + "USDT"
        
        // Initialize Bitget API client
        bitgetAPI := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        
        // Pre-warm cache with fast timeout (3 seconds max)
        log.Printf("🔄 Pre-warming balance cache for user %d...", user.UserID)
        go func() {
                if err := bitgetAPI.Cache.RefreshBalance(); err != nil {
                        log.Printf("⚠️ Balance pre-warm failed for user %d: %v (will check during order)", user.UserID, err)
                }
        }()
        
        // Small delay to let pre-warm complete if fast
        time.Sleep(200 * time.Millisecond)
        
        // Send notification to user
        tb.sendMessage(user.UserID, fmt.Sprintf("🚀 Auto-trade triggered for %s\nMargin: %.2f USDT\nLeverage: %dx\nOpening long position...", tradingSymbol, user.MarginUSDT, user.Leverage))
        
        // Record order sent timestamp
        orderSentAt := time.Now()
        
        // Execute long position
        result, err := bitgetAPI.OpenLongPosition(tradingSymbol, user.MarginUSDT, user.Leverage)
        
        // Record order confirmed timestamp
        orderConfirmedAt := time.Now()
        
        if err != nil {
                log.Printf("❌ Auto-trade failed for user %d on %s: %v", user.UserID, tradingSymbol, err)
                tb.sendMessage(user.UserID, fmt.Sprintf("❌ Auto-trade FAILED for %s: %v", tradingSymbol, err))
                return
        }

        log.Printf("✅ Auto-trade SUCCESS for user %d on %s", user.UserID, tradingSymbol)
        
        // Update trade execution log with Bitget timestamps
        if tb.upbitMonitor != nil {
                logEntry := tb.upbitMonitor.GetCurrentLogEntry(symbol)
                if logEntry != nil {
                        logEntry.UserID = user.UserID
                        logEntry.BitgetOrderSentAt = orderSentAt.Format("2006-01-02 15:04:05.000000")
                        logEntry.BitgetOrderConfirmed = orderConfirmedAt.Format("2006-01-02 15:04:05.000000")
                        
                        // Calculate latencies
                        detectedTime, _ := time.Parse("2006-01-02 15:04:05.000000", logEntry.UpbitDetectedAt)
                        savedTime, _ := time.Parse("2006-01-02 15:04:05.000000", logEntry.SavedToFileAt)
                        
                        logEntry.LatencyBreakdown["detection_to_file_ms"] = savedTime.Sub(detectedTime).Milliseconds()
                        logEntry.LatencyBreakdown["file_to_bitget_ms"] = orderSentAt.Sub(savedTime).Milliseconds()
                        logEntry.LatencyBreakdown["bitget_response_ms"] = orderConfirmedAt.Sub(orderSentAt).Milliseconds()
                        logEntry.LatencyBreakdown["total_execution_ms"] = orderConfirmedAt.Sub(detectedTime).Milliseconds()
                        
                        // Save log entry
                        if err := tb.upbitMonitor.appendTradeLog(logEntry); err != nil {
                                log.Printf("⚠️ Failed to save trade execution log: %v", err)
                        }
                }
        }
        
        // Send enhanced notification with P&L tracking
        tb.sendPositionNotification(user.UserID, result)
}

// Send message to user (helper method)
func (tb *TelegramBot) sendMessage(chatID int64, text string) {
        msg := tgbotapi.NewMessage(chatID, text)
        _, err := tb.bot.Send(msg)
        if err != nil {
                log.Printf("Failed to send message to %d: %v", chatID, err)
        }
}

// Create main menu keyboard
func (tb *TelegramBot) createMainMenu() tgbotapi.InlineKeyboardMarkup {
        return tgbotapi.NewInlineKeyboardMarkup(
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("📊 Bakiye", "balance"),
                        tgbotapi.NewInlineKeyboardButtonData("⚙️ Ayarlar", "settings"),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("🔧 Setup", "setup"),
                        tgbotapi.NewInlineKeyboardButtonData("❌ Pozisyonları Kapat", "close_all"),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("📈 Pozisyonlar", "positions"),
                        tgbotapi.NewInlineKeyboardButtonData("❓ Yardım", "help"),
                ),
        )
}

// Handle /start command
func (tb *TelegramBot) handleStart(chatID int64, userID int64, username string) {
        user, exists := tb.getUser(userID)
        if !exists {
                // Check if TEST mode and auto-setup first user
                if AutoSetupTestUser(tb, userID, username) {
                        msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(`✅ **TEST MODE - Auto-Configured!**

👤 User: @%s
💰 Margin: 50 USDT
📊 Leverage: 10x
🔑 Bitget API: TEST credentials

🧪 **Ready for testing!**

Use the menu below to test features:`, username))
                        msg.ParseMode = "Markdown"
                        msg.ReplyMarkup = tb.createMainMenu()
                        tb.bot.Send(msg)
                        return
                }
                
                // Create new user (normal mode)
                user = &UserData{
                        UserID:   userID,
                        Username: username,
                        IsActive: false,
                        State:    StateNone,
                }
                tb.saveUser(user)
        }

        welcomeMsg := fmt.Sprintf(`👋 **Hoşgeldin @%s!**

🚀 **Upbit-Bitget Otomatik Trading Botu**

Bu bot, Upbit'te listelenen yeni coinleri otomatik olarak Bitget'te long position ile alır.

**Nasıl Çalışır:**
1. Upbit'te yeni coin listesi açıklandığında
2. Bot otomatik olarak Bitget'te long position açar
3. Senin belirlediğin miktar ve leverage ile işlem yapar

**Ana Menü:** Aşağıdaki butonlardan istediğin işlemi seç:`, username)

        msg := tgbotapi.NewMessage(chatID, welcomeMsg)
        msg.ParseMode = "Markdown"
        msg.ReplyMarkup = tb.createMainMenu()
        tb.bot.Send(msg)
}

// Handle /setup command (start setup process)
func (tb *TelegramBot) handleSetup(chatID int64, userID int64, username string) {
        user, exists := tb.getUser(userID)
        
        // Eğer kullanıcı varsa ve API bilgileri kayıtlıysa
        if exists && user.BitgetAPIKey != "" {
                // Kayıtlı API bilgileri var, kullanıcıya sor
                confirmMsg := `🔧 **Setup Menüsü**

Kayıtlı API bilgileriniz bulundu.

**Ne yapmak istersiniz?**`

                keyboard := tgbotapi.NewInlineKeyboardMarkup(
                        tgbotapi.NewInlineKeyboardRow(
                                tgbotapi.NewInlineKeyboardButtonData("✏️ Sadece Margin/Leverage Değiştir", "setup_quick"),
                                tgbotapi.NewInlineKeyboardButtonData("🔄 API Bilgilerini Değiştir", "setup_full"),
                        ),
                        tgbotapi.NewInlineKeyboardRow(
                                tgbotapi.NewInlineKeyboardButtonData("« Geri", "main_menu"),
                        ),
                )

                msg := tgbotapi.NewMessage(chatID, confirmMsg)
                msg.ParseMode = "Markdown"
                msg.ReplyMarkup = keyboard
                tb.bot.Send(msg)
                
                user.State = StateConfirmAPIChange
                tb.saveUser(user)
        } else {
                // İlk setup veya API bilgileri yok, normal akış
                setupMsg := `🔧 **Bitget API Setup**

API bilgilerinizi adım adım girelim:

1️⃣ **Bitget API Key'inizi gönderin**

API bilgilerinizi Bitget > API Management bölümünden alabilirsiniz:
https://www.bitget.com/api-doc

⚠️ **Güvenlik:** Sensitive data güvenli şekilde saklanır.
⚠️ **İptal:** Setup'ı iptal etmek için /start yazın.`

                msg := tgbotapi.NewMessage(chatID, setupMsg)
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)

                if !exists {
                        user = &UserData{
                                UserID:   userID,
                                Username: username,
                                IsActive: false,
                                State:    StateAwaitingKey,
                        }
                } else {
                        user.State = StateAwaitingKey
                }
                
                tb.saveUser(user)
        }
}

// Handle /settings command
func (tb *TelegramBot) handleSettings(chatID int64, userID int64) {
        log.Printf("🔧 Settings called for user %d", userID)
        
        user, exists := tb.getUser(userID)
        if !exists {
                log.Printf("❌ User %d not found", userID)
                msg := tgbotapi.NewMessage(chatID, "❌ Henüz hiç kurulum yapmadınız. 🔧 Setup butonuna tıklayın.")
                msg.ReplyMarkup = tb.createMainMenu()
                tb.bot.Send(msg)
                return
        }
        
        if user.BitgetAPIKey == "" {
                log.Printf("❌ User %d has no API key", userID)
                msg := tgbotapi.NewMessage(chatID, "❌ Henüz API ayarlarını yapmadınız. 🔧 Setup butonuna tıklayın.")
                msg.ReplyMarkup = tb.createMainMenu()
                tb.bot.Send(msg)
                return
        }
        
        log.Printf("✅ Showing settings for user %d", userID)

        // Calculate risk level properly
        var riskLevel string
        if user.Leverage <= 5 {
                riskLevel = "🟢 Düşük"
        } else if user.Leverage <= 20 {
                riskLevel = "🟡 Orta"
        } else {
                riskLevel = "🔴 Yüksek"
        }

        // Safe API key preview
        var keyPreview string
        if len(user.BitgetAPIKey) >= 8 {
                keyPreview = user.BitgetAPIKey[:8] + "..."
        } else {
                keyPreview = strings.Repeat("*", len(user.BitgetAPIKey)) + "..."
        }

        // Plain text settings summary - no markdown issues
        settingsMsg := fmt.Sprintf(`⚙️ TRADING AYARLARINIZ

👤 HESAP BİLGİLERİ:
• Kullanıcı: @%s (ID: %d) 
• Durum: %s

💰 TRADE PARAMETRELERİ:
• Margin Miktarı: %.2f USDT
• Leverage Oranı: %dx  
• Risk Seviyesi: %s

🔐 API KONFIGÜRASYONU:
• API Key: %s
• Bağlantı Durumu: Aktif
• API Versiyonu: Bitget v2

🚀 AUTO-TRADING:
• UPBIT Monitoring: Aktif
• Otomatik İşlem: %s
• Pozisyon Yönetimi: Otomatik

💡 HIZLI İŞLEMLER:
🔧 Setup değiştir: /setup
📊 Bakiye gör: Ana menüden
📈 Pozisyonlar: Ana menüden`,
                user.Username,
                user.UserID,
                map[bool]string{true: "🟢 Aktif", false: "🔴 Pasif"}[user.IsActive],
                user.MarginUSDT,
                user.Leverage,
                riskLevel,
                keyPreview,
                map[bool]string{true: "🟢 Aktif", false: "🔴 Pasif"}[user.IsActive])

        log.Printf("📤 Creating plain text settings message for chat %d", chatID)
        msg := tgbotapi.NewMessage(chatID, settingsMsg)
        // NO MARKDOWN - plain text only
        msg.ReplyMarkup = tb.createMainMenu()
        
        log.Printf("📤 Sending settings message...")
        response, err := tb.bot.Send(msg)
        if err != nil {
                log.Printf("❌ Failed to send settings message: %v", err)
                // Try simpler message
                simpleMsg := tgbotapi.NewMessage(chatID, "⚙️ Settings error. Bot çalışıyor ama mesaj gönderemedi.")
                tb.bot.Send(simpleMsg)
        } else {
                log.Printf("✅ Settings message sent successfully! Message ID: %d", response.MessageID)
        }
}

// Handle /close command
func (tb *TelegramBot) handleClose(chatID int64, userID int64) {
        user, exists := tb.getUser(userID)
        if !exists || user.BitgetAPIKey == "" {
                msg := tgbotapi.NewMessage(chatID, "❌ API ayarlarını yapmadınız.")
                tb.bot.Send(msg)
                return
        }

        if !user.IsActive {
                msg := tgbotapi.NewMessage(chatID, "❌ Setup'ınız tamamlanmamış. /setup komutunu kullanın.")
                tb.bot.Send(msg)
                return
        }

        msg := tgbotapi.NewMessage(chatID, "🚨 Tüm pozisyonlarınız kapatılıyor...")
        tb.bot.Send(msg)

        // Close all positions using Bitget API
        tb.closeUserPositions(chatID, user)
}

// Close all positions for a user
func (tb *TelegramBot) closeUserPositions(chatID int64, user *UserData) {
        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        
        // Close all USDT futures positions
        resp, err := api.CloseAllPositions()
        if err != nil {
                errorMsg := fmt.Sprintf("❌ Pozisyon kapatma başarısız:\n%s", err.Error())
                msg := tgbotapi.NewMessage(chatID, errorMsg)
                tb.bot.Send(msg)
                return
        }

        // Clear all positions from tracking (thread-safe)
        positionsMutex.Lock()
        for positionKey := range activePositions {
                if strings.HasPrefix(positionKey, fmt.Sprintf("%d_", chatID)) {
                        delete(activePositions, positionKey)
                        log.Printf("🗑️ Removed position %s from tracking", positionKey)
                }
        }
        positionsMutex.Unlock()
        
        // Save updated positions to file
        go saveActivePositions()

        successMsg := fmt.Sprintf(`✅ **Pozisyonlar Başarıyla Kapatıldı**

📋 **Order ID:** %s
👤 **Kullanıcı:** @%s
💼 **Tüm USDT-Futures pozisyonlarınız kapatıldı.**

/settings - Ayarları görüntüle
/setup - Yeni ayarlar yap`, resp.OrderID, user.Username)

        msg := tgbotapi.NewMessage(chatID, successMsg)
        msg.ParseMode = "Markdown"
        tb.bot.Send(msg)
}

// Main message handler
func (tb *TelegramBot) handleMessage(update tgbotapi.Update) {
        if update.Message == nil {
                return
        }

        chatID := update.Message.Chat.ID
        userID := update.Message.From.ID
        username := update.Message.From.UserName
        text := update.Message.Text

        log.Printf("📨 Message from @%s (ID:%d): %s", username, userID, text)

        // Handle commands
        if update.Message.IsCommand() {
                switch update.Message.Command() {
                case "start":
                        tb.handleStart(chatID, userID, username)
                case "setup":
                        tb.handleSetup(chatID, userID, username)
                case "settings", "setting":  // Both /settings and /setting work
                        tb.handleSettings(chatID, userID)
                case "close":
                        tb.handleClose(chatID, userID)
                case "status":
                        msg := tgbotapi.NewMessage(chatID, "🤖 Bot aktif olarak çalışıyor!")
                        tb.bot.Send(msg)
                case "help":
                        tb.handleStart(chatID, userID, username) // Same as start
                default:
                        msg := tgbotapi.NewMessage(chatID, "❓ Bilinmeyen komut. /help komutunu deneyin.")
                        tb.bot.Send(msg)
                }
                return
        }

        // Handle non-command messages (setup process)
        tb.handleSetupProcess(chatID, userID, text)
}

// Handle setup process messages (API key, secret, etc.)
func (tb *TelegramBot) handleSetupProcess(chatID int64, userID int64, text string) {
        user, exists := tb.getUser(userID)
        if !exists || user.State == StateNone {
                return // User not in setup process
        }

        switch user.State {
        case StateAwaitingKey:
                user.BitgetAPIKey = strings.TrimSpace(text)
                user.State = StateAwaitingSecret
                tb.saveUser(user)
                
                msg := tgbotapi.NewMessage(chatID, "✅ API Key alındı!\n\n2️⃣ **Secret Key'inizi gönderin**")
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)

        case StateAwaitingSecret:
                user.BitgetSecret = strings.TrimSpace(text)
                user.State = StateAwaitingPasskey
                tb.saveUser(user)
                
                msg := tgbotapi.NewMessage(chatID, "✅ Secret Key alındı!\n\n3️⃣ **Passphrase'inizi gönderin**")
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)

        case StateAwaitingPasskey:
                user.BitgetPasskey = strings.TrimSpace(text)
                user.State = StateAwaitingMargin
                tb.saveUser(user)
                
                msg := tgbotapi.NewMessage(chatID, "✅ Passphrase alındı!\n\n4️⃣ **Margin tutarını USDT olarak gönderin**\nÖrnek: 100")
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)

        case StateAwaitingMargin:
                margin, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
                if err != nil || margin <= 0 {
                        msg := tgbotapi.NewMessage(chatID, "❌ Geçersiz tutar! Pozitif bir sayı girin (örn: 100)")
                        tb.bot.Send(msg)
                        return
                }
                
                user.MarginUSDT = margin
                user.State = StateAwaitingLeverage
                tb.saveUser(user)
                
                msg := tgbotapi.NewMessage(chatID, "✅ Margin tutarı alındı!\n\n5️⃣ **Leverage değerini gönderin**\nÖrnek: 10 (10x leverage için)")
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)

        case StateAwaitingLeverage:
                leverage, err := strconv.Atoi(strings.TrimSpace(text))
                if err != nil || leverage < 1 || leverage > 125 {
                        msg := tgbotapi.NewMessage(chatID, "❌ Geçersiz leverage! 1-125 arası bir sayı girin")
                        tb.bot.Send(msg)
                        return
                }
                
                user.Leverage = leverage
                user.State = StateComplete
                user.IsActive = true
                tb.saveUser(user)
                
                // Test API credentials
                tb.testUserAPI(chatID, user)

        default:
                // Reset to start if unknown state
                user.State = StateNone
                tb.saveUser(user)
        }
}

// Test user's Bitget API credentials
func (tb *TelegramBot) testUserAPI(chatID int64, user *UserData) {
        msg := tgbotapi.NewMessage(chatID, "🔍 API bağlantısı test ediliyor...")
        tb.bot.Send(msg)

        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        
        // Test API with account balance
        _, err := api.GetAccountBalance()
        if err != nil {
                user.IsActive = false
                user.State = StateNone
                tb.saveUser(user)
                
                errorMsg := fmt.Sprintf(`❌ **API Bağlantısı Başarısız**

Hata: %s

Lütfen API bilgilerinizi kontrol edip /setup ile tekrar deneyin.

**Kontrol Listesi:**
• API Key doğru mu?
• Secret Key doğru mu? 
• Passphrase doğru mu?
• API'da futures trading izni var mı?`, err.Error())

                msg := tgbotapi.NewMessage(chatID, errorMsg)
                msg.ParseMode = "Markdown"
                tb.bot.Send(msg)
                return
        }

        successMsg := fmt.Sprintf(`✅ **Setup Başarıyla Tamamlandı!**

👤 **Kullanıcı:** @%s
💰 **Margin:** %.2f USDT
📈 **Leverage:** %dx
🔐 **API:** Bağlantı başarılı
🎯 **Durum:** Aktif - Auto trading hazır!

🚀 **Bot artık Upbit'te yeni listelenen coinleri otomatik olarak Bitget'te long position ile alacak.**

**Komutlar:**
• /settings - Ayarları görüntüle
• /close - Tüm pozisyonları kapat
• /setup - Ayarları değiştir`, user.Username, user.MarginUSDT, user.Leverage)

        msg = tgbotapi.NewMessage(chatID, successMsg)
        msg.ParseMode = "Markdown"
        tb.bot.Send(msg)
}

// Start the bot
func (tb *TelegramBot) Start() {
        log.Printf("🤖 Telegram Bot starting...")

        updateConfig := tgbotapi.NewUpdate(0)
        updateConfig.Timeout = 60

        updates := tb.bot.GetUpdatesChan(updateConfig)

        for update := range updates {
                if update.Message != nil {
                        tb.handleMessage(update)
                } else if update.CallbackQuery != nil {
                        tb.handleCallbackQuery(update.CallbackQuery)
                }
        }
}

// Handle callback queries from inline keyboards
func (tb *TelegramBot) handleCallbackQuery(callback *tgbotapi.CallbackQuery) {
        // Answer the callback query to remove loading state
        callbackConfig := tgbotapi.NewCallback(callback.ID, "")
        tb.bot.Request(callbackConfig)

        chatID := callback.Message.Chat.ID
        userID := callback.From.ID
        data := callback.Data

        switch data {
        case "balance":
                tb.handleBalanceQuery(chatID, userID)
        case "settings":
                tb.handleSettings(chatID, userID)
        case "setup":
                tb.handleSetup(chatID, userID, callback.From.UserName)
        case "setup_quick":
                // Sadece margin/leverage değiştir
                user, exists := tb.getUser(userID)
                if exists {
                        user.State = StateAwaitingMargin
                        tb.saveUser(user)
                        
                        msg := tgbotapi.NewMessage(chatID, `✏️ **Hızlı Güncelleme**

4️⃣ **Margin tutarını USDT olarak gönderin**
Örnek: 100 (100 USDT için)

⚠️ Mevcut API bilgileriniz korunacak.`)
                        msg.ParseMode = "Markdown"
                        tb.bot.Send(msg)
                }
        case "setup_full":
                // API bilgilerini baştan al
                user, exists := tb.getUser(userID)
                if exists {
                        user.State = StateAwaitingKey
                        tb.saveUser(user)
                        
                        msg := tgbotapi.NewMessage(chatID, `🔄 **API Bilgilerini Yeniden Gir**

1️⃣ **Bitget API Key'inizi gönderin**

API bilgilerinizi Bitget > API Management bölümünden alabilirsiniz.

⚠️ **İptal:** Setup'ı iptal etmek için /start yazın.`)
                        msg.ParseMode = "Markdown"
                        tb.bot.Send(msg)
                }
        case "close_all":
                tb.handleClose(chatID, userID)
        case "positions":
                tb.handlePositionsQuery(chatID, userID)
        case "help":
                tb.handleHelpQuery(chatID)
        case "main_menu":
                tb.handleStart(chatID, userID, callback.From.UserName)
        default:
                if strings.HasPrefix(data, "close_position_") {
                        symbol := strings.TrimPrefix(data, "close_position_")
                        tb.handleCloseSpecificPosition(chatID, userID, symbol)
                }
        }
}

// Handle balance query
func (tb *TelegramBot) handleBalanceQuery(chatID int64, userID int64) {
        user, exists := tb.getUser(userID)
        if !exists || user.BitgetAPIKey == "" {
                tb.sendMessage(chatID, "❌ Henüz API ayarlarınızı yapmadınız. 🔧 Setup butonuna tıklayın.")
                return
        }

        if !user.IsActive {
                tb.sendMessage(chatID, "❌ Setup'ınız tamamlanmamış. 🔧 Setup butonuna tıklayın.")
                return
        }

        tb.sendMessage(chatID, "💰 Bakiye bilgileri alınıyor...")

        // Get balance using Bitget API
        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        balances, err := api.GetAccountBalance()
        if err != nil {
                tb.sendMessage(chatID, fmt.Sprintf("❌ Bakiye alınamadı: %v", err))
                return
        }

        balanceText := "📊 **Bakiye Bilgileri:**\n\n"
        if len(balances) == 0 {
                balanceText += "✅ Henüz bakiye bilgisi yok"
        } else {
                for _, balance := range balances {
                        availableFloat, _ := strconv.ParseFloat(balance.Available, 64)
                balanceText += fmt.Sprintf("💰 **%s**: %.2f USDT\n", balance.MarginCoin, availableFloat)
                }
        }

        balanceMsg := fmt.Sprintf(`💰 **Futures Bakiye**

%s

🔄 **Ana Menü için /start yazın**`, balanceText)

        msg := tgbotapi.NewMessage(chatID, balanceMsg)
        msg.ParseMode = "Markdown"
        msg.ReplyMarkup = tb.createMainMenu()
        tb.bot.Send(msg)
}

// Handle positions query
func (tb *TelegramBot) handlePositionsQuery(chatID int64, userID int64) {
        user, exists := tb.getUser(userID)
        if !exists || user.BitgetAPIKey == "" {
                tb.sendMessage(chatID, "❌ Henüz API ayarlarınızı yapmadınız. 🔧 Setup butonuna tıklayın.")
                return
        }

        if !user.IsActive {
                tb.sendMessage(chatID, "❌ Setup'ınız tamamlanmamış. 🔧 Setup butonuna tıklayın.")
                return
        }

        tb.sendMessage(chatID, "📈 Pozisyon bilgileri alınıyor...")

        // Get positions using Bitget API
        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        positions, err := api.GetAllPositions()
        if err != nil {
                tb.sendMessage(chatID, fmt.Sprintf("❌ Pozisyonlar alınamadı: %v", err))
                return
        }

        if len(positions) == 0 {
                msg := tgbotapi.NewMessage(chatID, "📈 **Pozisyonlar**\n\n✅ Şu anda açık pozisyon bulunmuyor.")
                msg.ParseMode = "Markdown"
                msg.ReplyMarkup = tb.createMainMenu()
                tb.bot.Send(msg)
                return
        }

        positionsText := "📊 **Açık Pozisyonlar:**\n\n"
        for _, pos := range positions {
                if pos.Size != "0" {
                        positionsText += fmt.Sprintf("💹 **%s** - Size: %s - PnL: %s\n", pos.Symbol, pos.Size, pos.UnrealizedPL)
                }
        }
        
        if positionsText == "📊 **Açık Pozisyonlar:**\n\n" {
                positionsText = "✅ Şu anda açık pozisyon bulunmuyor."
        }

        positionsMsg := fmt.Sprintf(`📈 **Açık Pozisyonlar**

%s

🔄 **Ana Menü için /start yazın**`, positionsText)

        msg := tgbotapi.NewMessage(chatID, positionsMsg)
        msg.ParseMode = "Markdown"
        msg.ReplyMarkup = tb.createMainMenu()
        tb.bot.Send(msg)
}

// Handle help query
func (tb *TelegramBot) handleHelpQuery(chatID int64) {
        helpMsg := `❓ **Yardım & Rehber**

🚀 **Bot Nasıl Çalışır:**
• Upbit'te yeni coin listelendiğinde otomatik tespit eder
• Sizin ayarlarınızla Bitget'te long position açar
• İşlem sonucunu size bildirir
• İstediğinizde pozisyonları kapatabilirsiniz

🔧 **Setup Süreci:**
1. 📊 Bakiye - Futures bakiyenizi görüntüleyin
2. ⚙️ Ayarlar - Mevcut ayarlarınızı kontrol edin
3. 🔧 Setup - API bilgilerinizi girin
4. ❌ Pozisyonları Kapat - Tüm pozisyonları kapatın

⚠️ **Önemli Uyarılar:**
• Bu bot gerçek parayla işlem yapar
• Sadece kaybetmeyi göze alabileceğiniz miktarla kullanın
• API bilgileriniz güvenli şekilde şifrelenir
• Leverage kullanımına dikkat edin

📞 **Destek:** @oxmtnslk ile iletişime geçin`

        msg := tgbotapi.NewMessage(chatID, helpMsg)
        msg.ParseMode = "Markdown"
        msg.ReplyMarkup = tb.createMainMenu()
        tb.bot.Send(msg)
}

// Handle closing specific position
func (tb *TelegramBot) handleCloseSpecificPosition(chatID int64, userID int64, symbol string) {
        user, exists := tb.getUser(userID)
        if !exists || user.BitgetAPIKey == "" {
                tb.sendMessage(chatID, "❌ API ayarlarınızı yapmadınız.")
                return
        }

        tb.sendMessage(chatID, fmt.Sprintf("🚨 %s pozisyonu kapatılıyor...", symbol))

        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        result, err := api.FlashClosePosition(symbol, "long")
        if err != nil {
                tb.sendMessage(chatID, fmt.Sprintf("❌ %s pozisyonu kapatılamadı: %v", symbol, err))
                return
        }

        // Remove specific position from tracking (thread-safe)
        positionKey := fmt.Sprintf("%d_%s", chatID, symbol)
        positionsMutex.Lock()
        if _, exists := activePositions[positionKey]; exists {
                delete(activePositions, positionKey)
                log.Printf("🗑️ Removed position %s from tracking", positionKey)
        }
        positionsMutex.Unlock()
        
        // Save updated positions to file
        go saveActivePositions()

        tb.sendMessage(chatID, fmt.Sprintf("✅ %s pozisyonu başarıyla kapatıldı!\n\nPozisyon ID: %s", symbol, result.OrderID))
}

// Send enhanced position notification with P&L tracking
func (tb *TelegramBot) sendPositionNotification(chatID int64, orderResp *OrderResponse) {
        // Calculate current P&L
        user, exists := tb.getUser(chatID)
        if !exists {
                return
        }
        
        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        currentPrice, err := api.GetSymbolPrice(orderResp.Symbol)
        if err != nil {
                currentPrice = orderResp.OpenPrice // Fallback to open price
        }
        
        // Calculate P&L: (CurrentPrice - OpenPrice) * Size
        priceChange := currentPrice - orderResp.OpenPrice
        priceChangePercent := (priceChange / orderResp.OpenPrice) * 100
        usdPnL := priceChange * orderResp.Size
        usdPnLWithLeverage := usdPnL * float64(orderResp.Leverage)
        
        // Format P&L colors
        pnlIcon := "🔴"
        pnlColor := "📉"
        if usdPnLWithLeverage > 0 {
                pnlIcon = "🟢"
                pnlColor = "📈"
        } else if usdPnLWithLeverage == 0 {
                pnlIcon = "⚪"
                pnlColor = "➡️"
        }
        
        notificationMsg := fmt.Sprintf(`🎉 Pozisyon Açıldı!

💹 Sembol: %s
📊 Açılış Fiyatı: $%.4f
💰 Güncel Fiyat: $%.4f
📏 Pozisyon Boyutu: %.8f
⚖️ Kaldıraç: %dx
💵 Marjin: %.2f USDT

%s Fiyat Değişimi: %+.4f (%.2f%%)
%s P&L: %+.2f USDT

⏰ Sonraki hatırlatma: 5 dakika
Pozisyon ID: %s`, 
                orderResp.Symbol,
                orderResp.OpenPrice,
                currentPrice,
                orderResp.Size,
                orderResp.Leverage,
                orderResp.MarginUSDT,
                pnlColor,
                priceChange,
                priceChangePercent,
                pnlIcon,
                usdPnLWithLeverage,
                orderResp.OrderID)

        // Create close position button
        closeButton := tgbotapi.NewInlineKeyboardMarkup(
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("❌ %s Pozisyonunu Kapat", orderResp.Symbol), fmt.Sprintf("close_position_%s", orderResp.Symbol)),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("📊 Bakiye", "balance"),
                        tgbotapi.NewInlineKeyboardButtonData("📈 Tüm Pozisyonlar", "positions"),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("🏠 Ana Menü", "main_menu"),
                ),
        )

        msg := tgbotapi.NewMessage(chatID, notificationMsg)
        msg.ReplyMarkup = closeButton
        tb.bot.Send(msg)
        
        // Store position for tracking and reminders (thread-safe)
        positionKey := fmt.Sprintf("%d_%s", chatID, orderResp.Symbol)
        positionsMutex.Lock()
        activePositions[positionKey] = &PositionInfo{
                UserID:      chatID,
                Symbol:      orderResp.Symbol,
                OrderID:     orderResp.OrderID,
                OpenPrice:   orderResp.OpenPrice,
                Size:        orderResp.Size,
                MarginUSDT:  orderResp.MarginUSDT,
                Leverage:    orderResp.Leverage,
                OpenTime:    time.Now(),
                LastReminder: time.Now(),
        }
        positionsMutex.Unlock()
        
        // Save positions to file
        go saveActivePositions()
        
        log.Printf("📝 Position %s tracked for user %d", positionKey, chatID)
}

// Start 5-minute position reminder system
func (tb *TelegramBot) startPositionReminders() {
        log.Printf("⏰ Starting position reminder system...")
        
        // Debug: Check initial state
        positionsMutex.RLock()
        log.Printf("🔍 Initial active positions count: %d", len(activePositions))
        for key, pos := range activePositions {
                log.Printf("📊 Found position: %s (opened %s ago)", key, time.Since(pos.OpenTime).Round(time.Second))
        }
        positionsMutex.RUnlock()
        
        ticker := time.NewTicker(5 * time.Minute)
        defer ticker.Stop()
        
        for range ticker.C {
                now := time.Now()
                log.Printf("🔔 Reminder ticker fired at %s", now.Format("15:04:05"))
                
                positionsMutex.Lock()
                log.Printf("🔍 Checking %d active positions for reminders", len(activePositions))
                for positionKey, position := range activePositions {
                        timeSinceLastReminder := now.Sub(position.LastReminder)
                        log.Printf("📊 Position %s: Last reminder %s ago (need 5min)", positionKey, timeSinceLastReminder.Round(time.Second))
                        
                        // Check if 5 minutes have passed since last reminder
                        if timeSinceLastReminder >= 5*time.Minute {
                                log.Printf("✅ Sending reminder for position %s", positionKey)
                                positionsMutex.Unlock() // Unlock before sending reminder to avoid deadlock
                                tb.sendPositionReminder(position)
                                positionsMutex.Lock()   // Re-lock to update LastReminder
                                // Re-check position still exists (could have been deleted)
                                if pos, exists := activePositions[positionKey]; exists {
                                        pos.LastReminder = now
                                        log.Printf("📢 Sent 5-min reminder for position %s", positionKey)
                                }
                        }
                }
                positionsMutex.Unlock()
        }
}

// Send position reminder with current P&L
func (tb *TelegramBot) sendPositionReminder(position *PositionInfo) {
        user, exists := tb.getUser(position.UserID)
        if !exists {
                return
        }
        
        api := NewBitgetAPI(user.BitgetAPIKey, user.BitgetSecret, user.BitgetPasskey)
        
        // Get REAL position data from Bitget (accurate P&L like position display)
        positions, err := api.GetAllPositions()
        var realPnL float64 = 0
        var currentPrice float64 = position.OpenPrice
        
        if err != nil {
                log.Printf("⚠️ Could not get positions for reminder: %v", err)
                // Fallback to price lookup only
                currentPrice, _ = api.GetSymbolPrice(position.Symbol)
        } else {
                // Log all available positions for debugging
                log.Printf("🔍 Available positions from Bitget API:")
                for _, pos := range positions {
                        if pos.Size != "0" {
                                log.Printf("   📊 %s - Size: %s - PnL: %s", pos.Symbol, pos.Size, pos.UnrealizedPL)
                        }
                }
                
                // Find the specific position with flexible symbol matching
                var foundPosition *BitgetPosition
                for _, pos := range positions {
                        if pos.Size != "0" {
                                // Exact match first
                                if pos.Symbol == position.Symbol {
                                        foundPosition = &pos
                                        break
                                }
                                // Flexible matching: check if stored symbol is contained in API symbol
                                if strings.Contains(pos.Symbol, position.Symbol) {
                                        foundPosition = &pos
                                        log.Printf("🔄 Flexible match: %s contains %s", pos.Symbol, position.Symbol)
                                }
                                // Also check the reverse
                                if strings.Contains(position.Symbol, pos.Symbol) {
                                        foundPosition = &pos
                                        log.Printf("🔄 Reverse match: %s contains %s", position.Symbol, pos.Symbol)
                                }
                        }
                }
                
                if foundPosition != nil {
                        if pnlFloat, err := strconv.ParseFloat(foundPosition.UnrealizedPL, 64); err == nil {
                                realPnL = pnlFloat
                                log.Printf("🎯 Using REAL P&L for %s (matched %s): %.5f USDT", position.Symbol, foundPosition.Symbol, realPnL)
                        }
                        if priceFloat, err := strconv.ParseFloat(foundPosition.MarkPrice, 64); err == nil {
                                currentPrice = priceFloat
                        }
                } else {
                        // POSITION NOT FOUND IN BITGET - User closed it manually on exchange
                        log.Printf("🚨 Position %s not found in Bitget API - User closed it manually!", position.Symbol)
                        
                        // Remove from active positions
                        positionKey := fmt.Sprintf("%d_%s", position.UserID, position.Symbol)
                        positionsMutex.Lock()
                        delete(activePositions, positionKey)
                        positionsMutex.Unlock()
                        
                        // Save updated positions
                        go saveActivePositions()
                        
                        // Notify user that position was closed and tracking stopped
                        closedMsg := fmt.Sprintf(`✅ Pozisyon Kapandı

📊 %s pozisyonunuz Bitget'te kapalı durumda.

Bu pozisyon için hatırlatıcılar otomatik olarak durduruldu ve takip listesinden çıkarıldı.

💡 Yeni coin listelemelerini beklemeye devam ediyoruz!`, position.Symbol)
                        
                        msg := tgbotapi.NewMessage(position.UserID, closedMsg)
                        tb.bot.Send(msg)
                        
                        log.Printf("🗑️ Position %s removed from tracking (closed on exchange)", positionKey)
                        return // Stop processing this reminder
                }
        }
        
        // Calculate duration
        duration := time.Since(position.OpenTime)
        
        // Calculate price change for display
        priceChange := currentPrice - position.OpenPrice
        priceChangePercent := (priceChange / position.OpenPrice) * 100
        
        // Format P&L colors and icons (using REAL P&L from exchange)
        pnlIcon := "🔴"
        pnlColor := "📉"
        statusEmoji := "⚠️"
        if realPnL > 0 {
                pnlIcon = "🟢"
                pnlColor = "📈" 
                statusEmoji = "✅"
        } else if realPnL == 0 {
                pnlIcon = "⚪"
                pnlColor = "➡️"
                statusEmoji = "⏸️"
        }
        
        reminderMsg := fmt.Sprintf(`⏰ Pozisyon Hatırlatması
        
%s %s Pozisyonu Aktif

📊 Açılış: $%.4f
💰 Güncel: $%.4f  
⚖️ Kaldıraç: %dx
⏳ Süre: %s

%s Fiyat Değişimi: %+.4f (%.2f%%)
%s Güncel P&L: %+.2f USDT

Pozisyonunuzu istediğiniz zaman kapatabilirsiniz:`,
                statusEmoji,
                position.Symbol,
                position.OpenPrice,
                currentPrice,
                position.Leverage,
                formatDuration(duration),
                pnlColor,
                priceChange,
                priceChangePercent,
                pnlIcon,
                realPnL)
        
        // Create close position button
        closeButton := tgbotapi.NewInlineKeyboardMarkup(
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("❌ %s Pozisyonunu Kapat", position.Symbol), fmt.Sprintf("close_position_%s", position.Symbol)),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("📊 Bakiye", "balance"),
                        tgbotapi.NewInlineKeyboardButtonData("📈 Tüm Pozisyonlar", "positions"),
                ),
                tgbotapi.NewInlineKeyboardRow(
                        tgbotapi.NewInlineKeyboardButtonData("🔕 Hatırlatıcıyı Durdur", fmt.Sprintf("stop_reminders_%s", position.Symbol)),
                ),
        )
        
        msg := tgbotapi.NewMessage(position.UserID, reminderMsg)
        msg.ReplyMarkup = closeButton
        tb.bot.Send(msg)
}

// Format duration to human readable format
func formatDuration(d time.Duration) string {
        if d.Hours() >= 1 {
                return fmt.Sprintf("%.0fs %.0fd", d.Hours(), d.Minutes()-d.Hours()*60)
        }
        return fmt.Sprintf("%.0fd", d.Minutes())
}

// Save active positions to file
func saveActivePositions() {
        positionsMutex.RLock()
        defer positionsMutex.RUnlock()
        
        file, err := os.Create(positionsFile)
        if err != nil {
                log.Printf("⚠️ Could not save positions: %v", err)
                return
        }
        defer file.Close()
        
        encoder := json.NewEncoder(file)
        encoder.SetIndent("", "  ")
        if err := encoder.Encode(activePositions); err != nil {
                log.Printf("⚠️ Could not encode positions: %v", err)
        } else {
                log.Printf("💾 Saved %d active positions to file", len(activePositions))
        }
}

// Load active positions from file
func loadActivePositions() {
        file, err := os.Open(positionsFile)
        if err != nil {
                log.Printf("ℹ️ No saved positions file found (normal on first run)")
                return
        }
        defer file.Close()
        
        var savedPositions map[string]*PositionInfo
        decoder := json.NewDecoder(file)
        if err := decoder.Decode(&savedPositions); err != nil {
                log.Printf("⚠️ Could not decode saved positions: %v", err)
                return
        }
        
        positionsMutex.Lock()
        activePositions = savedPositions
        positionsMutex.Unlock()
        
        log.Printf("📂 Loaded %d active positions from file", len(savedPositions))
        for key, pos := range savedPositions {
                log.Printf("📊 Restored position: %s (opened %s ago)", key, time.Since(pos.OpenTime).Round(time.Second))
        }
}

// SetUpbitMonitor sets the upbit monitor reference for trade logging
func (tb *TelegramBot) SetUpbitMonitor(monitor *UpbitMonitor) {
        tb.upbitMonitor = monitor
}

// InitializeTelegramBot creates and returns bot instance (called from main.go)
func InitializeTelegramBot() *TelegramBot {
        // Check for TEST token first (for isolated testing)
        token := os.Getenv("TEST_TELEGRAM_BOT_TOKEN")
        if token == "" {
                // Fallback to production token if TEST not found
                token = os.Getenv("TELEGRAM_BOT_TOKEN")
                log.Printf("ℹ️ Using PRODUCTION Telegram Bot Token")
        } else {
                log.Printf("🧪 Using TEST Telegram Bot Token (test mode)")
        }
        
        if token == "" {
                log.Fatal("TELEGRAM_BOT_TOKEN or TEST_TELEGRAM_BOT_TOKEN environment variable is required")
        }

        bot, err := NewTelegramBot(token)
        if err != nil {
                log.Fatalf("Failed to create bot: %v", err)
        }
        
        // Initialize test user if in test mode
        InitializeTestUser(bot)

        log.Printf("🚀 Multi-User Upbit-Bitget Auto Trading Bot initialized")
        return bot
}

// ExecuteAutoTradeForAllUsers triggers auto-trading for all active users (INSTANT callback)
func (tb *TelegramBot) ExecuteAutoTradeForAllUsers(symbol string) {
        log.Printf("⚡ INSTANT EXECUTION - New listing detected: %s", symbol)
        
        // Check for duplicate (prevent double execution)
        if symbol == tb.lastProcessedSymbol {
                log.Printf("🔄 Symbol %s already processed via instant callback, skipping", symbol)
                return
        }
        
        tb.lastProcessedSymbol = symbol
        
        // Get all active users
        activeUsers := tb.getAllActiveUsers()
        if len(activeUsers) == 0 {
                log.Printf("⚠️  No active users found for auto-trading")
                return
        }

        log.Printf("⚡ FAST TRACK: Executing trades for %d users on %s", len(activeUsers), symbol)

        // Execute trades in parallel for speed
        for _, user := range activeUsers {
                go tb.executeAutoTrade(user, symbol)
        }
}

// StartTradingBot starts the trading bot (to be called from main.go)
func StartTradingBot() {
        bot := InitializeTelegramBot()
        bot.Start()
}

// Start 4-hour status notification system
func (tb *TelegramBot) startStatusNotifications() {
        log.Printf("📢 Starting 4-hour status notification system...")
        
        // Her 4 saatte bir çalış
        ticker := time.NewTicker(4 * time.Hour)
        defer ticker.Stop()
        
        // Yaratıcı ve esprili mesajlar
        messages := []string{
                `🚀 **Robot Nöbetinde!**

⚡ Sistem 7/24 Upbit'i taramaya devam ediyor
💎 Yeni listing → Otomatik alım modu hazır
🎯 Her şey kontrolümüz altında
💸 Şimdi rahat uyu, biz hallederiz!`,

                `💰 **Para Avcısı Aktif!**

🔥 300ms hızda listing tespiti yapıyoruz
🎲 Sistem tam gaz çalışıyor
⚡ Bir listing çıksa da vursak...
😎 Bot modunda beklemede!`,

                `🎯 **Sniper Mode ON!**

✨ Ultra-hızlı tespit sistemi hazırda bekliyor
💎 Yeni coin listingi = Bizim fırsatımız
🚀 Proxy'ler döndürüyor, sistem stabil
🤑 Next opportunity loading...`,

                `⚡ **Speed Demon Aktif!**

🔍 Upbit'in her duyurusunu 0.9 saniyede yakalıyoruz
💸 Listing anı = Action time!
🎰 Şans oyunu değil, hız oyunu!
💪 Sistem full throttle çalışıyor!`,

                `🏎️ **Hız Sınırı Yok!**

🚀 Sub-second detection sistemi online
💎 Proxy rotation: Check ✅
⚡ Auto-trade ready: Check ✅
🔥 Biz her zaman bir adım önündeyiz!`,

                `🎮 **Game Mode: GRINDING**

💰 Upbit listing farmlamaya devam
🎯 Bot sistemi kesintisiz çalışıyor
⚡ Yeni coin → Instant execution
😏 AFK para kasıyoruz pratik olarak!`,

                `🌙 **Gece Nöbetçisi Burada!**

✨ İster uyku, ister iş - bot her zaman aktif
💎 Listing detection: Never sleeps
🚀 Para kazanma fırsatı kaçmaz
💪 24/7 grind mode activated!`,

                `🔥 **Full Send Modu!**

⚡ Sistem hazır, finger on the trigger
💰 Next Upbit listing bizim target
🎯 No hesitation, pure execution
🚀 Ready to catch that moon shot!`,

                `💎 **Diamond Hands Bot!**

✨ Sabırla bekliyor, hızla vuruyor
🎲 Timing is everything, we got it
⚡ Listing anında → Lightning fast entry
💸 Para makinesi standby modunda!`,

                `🎪 **Show Must Go On!**

🚀 Bot performansı: Oscar worthy
💰 Listing hunt devam ediyor
⚡ Non-stop action, zero chill
🔥 We don't stop, we just reload!`,
        }
        
        messageIndex := 0
        
        for {
                select {
                case <-ticker.C:
                        log.Printf("📢 4-hour status notification triggered")
                        
                        // Tüm aktif kullanıcılara mesaj gönder
                        tb.database.mutex.Lock()
                        activeUsers := 0
                        for _, user := range tb.database.Users {
                                if user.IsActive && user.BitgetAPIKey != "" {
                                        activeUsers++
                                        // Mesajı gönder
                                        msg := tgbotapi.NewMessage(user.UserID, messages[messageIndex])
                                        msg.ParseMode = "Markdown"
                                        tb.bot.Send(msg)
                                        
                                        // Rate limiting için kısa bekleme
                                        time.Sleep(100 * time.Millisecond)
                                }
                        }
                        tb.database.mutex.Unlock()
                        
                        // Bir sonraki mesaja geç (döngüsel)
                        messageIndex = (messageIndex + 1) % len(messages)
                        
                        log.Printf("📢 Status notification sent to %d active users", activeUsers)
                }
        }
}

// InitializeTestUser creates test user with predefined credentials
func InitializeTestUser(bot *TelegramBot) {
        // Check if TEST credentials exist
        testAPIKey := os.Getenv("TEST_BITGET_API_KEY")
        testSecret := os.Getenv("TEST_BITGET_SECRET")
        testPassphrase := os.Getenv("TEST_BITGET_PASSPHRASE")
        
        if testAPIKey == "" || testSecret == "" || testPassphrase == "" {
                log.Printf("⚠️ TEST credentials not found, skipping test user initialization")
                return
        }
        
        log.Printf("🧪 TEST CREDENTIALS DETECTED - Initializing test user...")
        
        // Use a test user ID (you'll need to get this from your test bot)
        // For now, we'll check if any user exists
        bot.database.mutex.RLock()
        existingUsers := len(bot.database.Users)
        bot.database.mutex.RUnlock()
        
        if existingUsers > 0 {
                log.Printf("ℹ️ Users already exist in database, skipping auto-init")
                log.Printf("💡 Send /setup to your test bot to configure manually")
                return
        }
        
        log.Printf("📝 No users found - waiting for you to send /start to test bot...")
        log.Printf("🤖 After /start, I'll auto-configure with test credentials:")
        log.Printf("   - Margin: 50 USDT")
        log.Printf("   - Leverage: 10x")
        log.Printf("   - Bitget API: TEST credentials")
}

// AutoSetupTestUser auto-configures first user with test credentials
func AutoSetupTestUser(bot *TelegramBot, userID int64, username string) bool {
        testAPIKey := os.Getenv("TEST_BITGET_API_KEY")
        testSecret := os.Getenv("TEST_BITGET_SECRET")
        testPassphrase := os.Getenv("TEST_BITGET_PASSPHRASE")
        
        if testAPIKey == "" {
                return false // Not in test mode
        }
        
        log.Printf("🧪 AUTO-SETUP: Configuring user %d with TEST credentials...", userID)
        
        // Encrypt credentials
        encAPIKey, _ := bot.encryptSensitiveData(testAPIKey)
        encSecret, _ := bot.encryptSensitiveData(testSecret)
        encPassphrase, _ := bot.encryptSensitiveData(testPassphrase)
        
        user := &UserData{
                UserID:        userID,
                Username:      username,
                BitgetAPIKey:  encAPIKey,
                BitgetSecret:  encSecret,
                BitgetPasskey: encPassphrase,
                MarginUSDT:    50.0,
                Leverage:      10,
                IsActive:      true,
                State:         StateComplete,
                CreatedAt:     time.Now().Format(time.RFC3339),
                UpdatedAt:     time.Now().Format(time.RFC3339),
        }
        
        bot.database.mutex.Lock()
        bot.database.Users[userID] = user
        bot.database.mutex.Unlock()
        bot.saveDatabase()
        
        log.Printf("✅ TEST USER CONFIGURED:")
        log.Printf("   - User ID: %d", userID)
        log.Printf("   - Username: %s", username)
        log.Printf("   - Margin: 50 USDT")
        log.Printf("   - Leverage: 10x")
        log.Printf("   - API: TEST credentials")
        
        return true
}

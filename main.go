package main

import (
        "fmt"
        "log"
        "time"

        "github.com/joho/godotenv"
)

// waitForNextMinute waits until the start of the next minute (XX:XX:00.000)
// This ensures all monitoring starts synchronized at 00 seconds
func waitForNextMinute() {
        // 1. Get current time
        now := time.Now()

        // 2. Calculate the start of the next minute
        //    Truncate(time.Minute) -> gives the start of current minute (e.g., 10:45:23 -> 10:45:00)
        //    Add(time.Minute)      -> adds 1 minute to get next minute start (e.g., 10:45:00 -> 10:46:00)
        nextMinute := now.Truncate(time.Minute).Add(time.Minute)

        // 3. Calculate duration until next minute starts
        //    time.Until(targetTime) -> returns duration from now until targetTime
        waitDuration := time.Until(nextMinute)

        // Log synchronization info
        fmt.Printf("⏰ Şu anki zaman: %s\n", now.Format("15:04:05.000"))
        fmt.Printf("🎯 Hedeflenen başlangıç zamanı: %s\n", nextMinute.Format("15:04:05.000"))
        fmt.Printf("⏳ %s boyunca bekleniyor...\n", waitDuration)

        // 4. Sleep until next minute starts
        time.Sleep(waitDuration)

        // Log start time
        fmt.Printf("🚀 Başlangıç! Zaman: %s\n", time.Now().Format("15:04:05.000"))
}

func main() {
        log.Println("🚀 Upbit-Bitget Auto Trading System Starting...")

        _ = godotenv.Load()
        
        // Wait for next minute to start (synchronized timing)
        waitForNextMinute()

        // Start Telegram bot first to get bot instance
        telegramBot := InitializeTelegramBot()
        
        // Create Upbit monitor with DIRECT callback to trading
        upbitMonitor := NewUpbitMonitor(func(symbol string) {
                log.Printf("🔥 INSTANT CALLBACK - New Upbit listing: %s", symbol)
                // DIRECT execution - no file delay!
                go telegramBot.ExecuteAutoTradeForAllUsers(symbol)
        })
        
        // Link monitor to bot for trade logging
        telegramBot.SetUpbitMonitor(upbitMonitor)

        log.Println("✅ All systems initialized")
        
        // TIME SYNCHRONIZATION CHECK
        log.Println("⏰ Checking time synchronization with exchanges...")
        
        // Check Upbit time sync
        upbitSync, err := upbitMonitor.GetServerTime()
        if err != nil {
                log.Printf("⚠️ Upbit time sync failed: %v", err)
        } else {
                log.Printf("📡 UPBIT TIME SYNC:")
                log.Printf("   • Server Time: %s", upbitSync.ServerTime.Format("2006-01-02 15:04:05.000"))
                log.Printf("   • Local Time:  %s", upbitSync.LocalTime.Format("2006-01-02 15:04:05.000"))
                log.Printf("   • Clock Offset: %v", upbitSync.ClockOffset)
                log.Printf("   • Network Latency: %v", upbitSync.NetworkLatency)
                
                if upbitSync.ClockOffset.Abs() > 1*time.Second {
                        log.Printf("⚠️ WARNING: Clock offset > 1s! May cause timing issues!")
                } else {
                        log.Printf("   ✅ Clock sync OK (offset < 1s)")
                }
        }
        
        // Check Bitget time sync (create temporary API instance)
        testBitget := NewBitgetAPI("test", "test", "test")
        bitgetSync, err := testBitget.GetServerTime()
        if err != nil {
                log.Printf("⚠️ Bitget time sync failed: %v", err)
        } else {
                log.Printf("📡 BITGET TIME SYNC:")
                log.Printf("   • Server Time: %s", bitgetSync.ServerTime.Format("2006-01-02 15:04:05.000"))
                log.Printf("   • Local Time:  %s", bitgetSync.LocalTime.Format("2006-01-02 15:04:05.000"))
                log.Printf("   • Clock Offset: %v", bitgetSync.ClockOffset)
                log.Printf("   • Network Latency: %v", bitgetSync.NetworkLatency)
                
                if bitgetSync.ClockOffset.Abs() > 1*time.Second {
                        log.Printf("⚠️ WARNING: Clock offset > 1s! May cause timing issues!")
                } else {
                        log.Printf("   ✅ Clock sync OK (offset < 1s)")
                }
        }
        
        log.Println("📡 Starting Upbit monitor...")
        log.Println("🤖 Starting Telegram bot...")

        go upbitMonitor.Start()

        // Start bot message loop
        telegramBot.Start()
}

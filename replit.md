# Overview

This project is a cryptocurrency trading bot designed to detect new coin listings on the Upbit exchange and automatically execute trades on the Bitget exchange. Its primary purpose is to leverage rapid listing detection (achieving ~0.75-0.9 second detection) for profitable trading opportunities. Key capabilities include a robust random proxy rotation system to bypass rate limits, a 5-rule filtering system for accurate listing detection, automated time synchronization, microsecond-precision trade logging, and multi-user support with duplicate trade prevention. The bot aims to capitalize on market inefficiencies arising from new listings with high-speed, automated execution.

# User Preferences

Preferred communication style: Simple, everyday language.

# System Architecture

## Core Components

### Listing Detection System
This system is designed to detect new cryptocurrency listings on Upbit in real-time, employing a random proxy rotation strategy. It uses a pool of 22 SOCKS5 proxies, randomly selecting one for each check at a configurable interval (production: 300ms, achieving 3.33 req/sec). An auto-blacklist system handles 429 errors by temporarily blacklisting proxies for 30 seconds. Detection is optimized with ETag tracking and a 5-rule filtering system to ensure 100% accurate listing identification and prevent false positives. Duplicate prevention is handled by a 2-layer caching mechanism.

### Trading Execution Engine
The bot executes trades automatically upon listing detection, leveraging parallel API calls to Bitget. It supports multi-user parallel goroutines, allowing all users to trade simultaneously. Configuration includes per-user margin and leverage settings, with order placement on Bitget futures/spot markets. This parallel execution reduces the time from detection to order placement to 0.5-0.8 seconds.

### User Management System
A JSON-based system (`bot_users.json`) manages multiple users, storing individual Bitget API credentials (encrypted/encoded), trading parameters (margin, leverage), and activation status. Telegram user IDs serve as primary identifiers, and a state machine tracks user configuration progress.

### Telegram Bot Interface
A Telegram bot facilitates user interaction for registration, API key configuration, trading parameter setup, and bot activation/deactivation. This provides a mobile-friendly and notification-rich interface for users.

## Data Storage

File-based persistence is used for `bot_users.json` (user profiles), `upbit_new.json` (historical listing detections), and `active_positions.json` (real-time position tracking with auto-sync to Bitget every 5 minutes). This approach offers simplicity and human readability for a single-instance bot.

## Security Architecture

API credentials are stored encoded/encrypted and isolated per user. Risk controls include per-user leverage limits, configurable margin amounts, and a manual activation flag.

## Integration Architecture

The system integrates with the Upbit public announcements API (using ETag for efficiency and handling Korean titles), Bitget API for authenticated futures/spot trading, and the Telegram Bot API for user interaction and control.

## Concurrency & Performance

The system utilizes parallel proxy workers, where each proxy runs in a separate goroutine with staggered starts to prevent rate limit issues. This architecture achieves significantly faster detection times. A 5-minute reminder system for active positions includes auto-sync with Bitget to validate and clean up closed positions, preventing "phantom reminders." Robust error handling is implemented for proxy failures and API errors.

# External Dependencies

## Third-Party Services

*   **Upbit Exchange**: Source for new coin listing announcements via its public announcements API endpoint (`/v1/notices`).
*   **Bitget Exchange**: Execution platform for automated trades using its Futures/Spot trading APIs, requiring API Key, Secret, and Passphrase for authentication.
*   **Telegram Bot API**: Provides the user interface and control mechanism for the bot, authenticated via a bot token.

## Proxy Infrastructure

*   **SOCKS5 Proxies**: Used for rate limit avoidance and IP distribution, configured via Go's proxy package.

## Programming Language & Frameworks

*   **Go (Golang)**: The core language (Go 1.x), utilizing standard packages like `net/http`, `golang.org/x/net/proxy`, `encoding/json`, and `regexp`.
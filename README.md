# vpc-score-agent

A lightweight background agent that bridges VPX plugin score events to the VPC Data API. Runs silently alongside Virtual Pinball X while a table is being played, forwarding game events in real-time with near-zero CPU and memory overhead.

---

## How it works

1. PinUp Popper launches `vpc-score-agent.exe` when a table starts, passing the table's VPS ID, version, and ROM name as arguments
2. The agent connects to the VPX plugin's local WebSocket on port 3123
3. Each event received (game_start, current_scores, game_end) is enriched with the player's identity and table context, then forwarded to the VPC Data API
4. If the upstream connection drops, events are buffered locally and flushed when connectivity resumes
5. PinUp Popper's close script signals the agent to stop cleanly when the table exits

---

## Prerequisites

- [PinUp Popper](https://www.nailbuster.com/wikipinup/doku.php) installed and configured
- VPS CSV imported into PinUp Popper (see setup below)
- VPX score plugin installed and running (provides the local WebSocket on port 3123)
- Your Discord user ID and username (see Identity Setup below)

---

## Identity Setup

The agent identifies you by your Discord user ID and username. These are configured once in PinUp Popper as global emulator variables and substituted into every table launch automatically.

**To find your Discord user ID:**

1. Open Discord settings → Advanced → Enable Developer Mode
2. Right-click your username anywhere → Copy User ID

---

## PinUp Popper Setup

### Step 1: Import the VPS CSV

The VPS CSV populates PinUp Popper's table metadata including the VPS ID and version number fields used by the agent.

1. Download the latest VPS CSV from [Virtual Pinball Spreadsheet](https://virtual-pinball-spreadsheet.web.app/)
2. In PinUp Popper, open **PinUP Popper Setup** → **GameManager**
3. Use the **Import** function to import the VPS CSV
4. Verify that **CUSTOM2** is populated with VPS IDs and **GAMEVER** contains version numbers for your tables

> The agent reads `CUSTOM2` for `vpsId` and `GAMEVER` for `versionNumber`. If your PinUp Popper installation uses different custom fields for VPS data, update the substitution variables in the scripts below accordingly.

### Step 2: Configure the Emulator Launch Scripts

In PinUp Popper, navigate to **PinUP Popper Setup** → **Emulators** → select your **Visual Pinball X** emulator → **Launch Scripts**.

#### Open Script (runs when a table launches)

Add the following line to your emulator-level open script. PinUp Popper substitutes the bracketed variables at launch time with the actual values for the selected table.

```bat
start "" "C:\vPinball\vpc-score-agent\vpc-score-agent.exe" --vpsId "[CUSTOM2]" --version "[GAMEVER]" --rom "[ROMNAME]" --userId "YOUR_DISCORD_USER_ID" --username "your_discord_username"
```

Replace:

- `C:\vPinball\vpc-score-agent\` with the actual path where you placed `vpc-score-agent.exe`
- `YOUR_DISCORD_USER_ID` with your Discord user ID (e.g. `123456789012345678`)
- `your_discord_username` with your Discord username in lowercase (e.g. `apollo`)

The `[CUSTOM2]`, `[GAMEVER]`, and `[ROMNAME]` variables are substituted automatically by PinUp Popper — do not replace these.


#### Close Script (runs when a table exits)

Add the following line to your emulator-level close script. This signals the running agent to stop cleanly.

```bat
"C:\vPinball\vpc-score-agent\vpc-score-agent.exe" --stop
```

The `--stop` flag writes a sentinel file that the running agent detects within 2 seconds, flushes any buffered events, and exits.

---

## Event buffering

If the VPC Data API is unreachable (e.g. no internet connection), events are held in memory and flushed automatically every 10 seconds once connectivity resumes. Up to 500 events are buffered — sufficient for any normal play session.

---

## Logging

The agent writes logs to `vpc-score-agent.log` in the same directory as the executable, with rotation at 10MB. Logs include connection status, every event received and forwarded, and any errors encountered.

---

## Building from source

Requires Go 1.22+. No external dependencies — standard library only.

```bash
# Build for Windows from any platform
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o vpc-score-agent.exe .

# Build for current platform (for testing)
go build -o vpc-score-agent .
```

Releases are built automatically by GitHub Actions on version tags (`v*.*.*`) and attached to the GitHub Release as `vpc-score-agent.exe`.

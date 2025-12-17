# MariaDB Replication Monitor

A high-performance Go application for monitoring and automatically fixing MariaDB replication issues across multiple database targets.

## Features

- **Multi-target monitoring** - Monitor multiple MariaDB slave servers concurrently with independent goroutines
- **Automatic error correction** - Handles common replication errors automatically:
  - Error 1062 (duplicate key) - Skips the problematic transaction
  - Error 1942 (parallel replication) - Optimizes parallel replication settings
  - Position mismatch - Resets master log position when Exec_Master_Log_Pos > Read_Master_Log_Pos
- **Replication lag tracking** - Real-time monitoring of replication progress with visual indicators:
  - ✓ Caught up
  - ↑ Catching up
  - ↓ Falling behind
  - → Stable
- **Intelligent backoff** - Reduces database load when no errors are detected, with incremental backoff up to 15 seconds
- **Connection reuse** - Maintains persistent connections for optimal performance
- **Structured logging** - Timestamp-prefixed, parseable log format for easy monitoring and alerting

## Installation

### Prerequisites

- Go 1.25.5 or later
- MariaDB/MySQL server with replication configured

### Build from source

#### Using Make (recommended)

The project includes a Makefile for easy cross-platform builds:

```bash
# Install dependencies
make deps

# Build for your current platform
make dev

# Build for specific platforms
make amd64    # Linux AMD64 (default)
make arm64    # Linux ARM64
make mactel   # macOS Intel
make macarm   # macOS ARM (M1/M2/M3)
make wintel   # Windows AMD64
make winarm   # Windows ARM64

# Build for all platforms
make all

# Clean build artifacts
make clean

# Show all available targets
make help
```

Binaries are placed in `bin/<os>/<arch>/` directories.

#### Manual build

```bash
go mod download
go build -o maria_repl_check
```

#### Manual cross-compile

```bash
GOOS=linux GOARCH=amd64 go build -o maria_repl_check
```

## Configuration

Create a `config.ini` file with your database targets (a sample is provided as `config.ini.sample`):

```ini
[defaults]
slave_parallel_max_queued = 262144
slave_parallel_threads = 3
slave_domain_parallel_threads = 2
master_log_pos = 4
max_backoff_seconds = 15
backoff_success_count = 5

[target1]
host = 192.168.1.100
port = 3306
username = root
password = your_password_here

[target2]
host = 192.168.1.101
port = 3306
username = root
password = your_password_here
```

### Configuration sections

**[defaults]** (optional) - Global settings that apply to all targets:
- `slave_parallel_max_queued` - Max queued bytes for parallel replication (default: 262144)
- `slave_parallel_threads` - Number of parallel threads (default: 3)
- `slave_domain_parallel_threads` - Domain parallel threads (default: 2)
- `master_log_pos` - Position to reset to on replication interruption (default: 4)
- `max_backoff_seconds` - Maximum backoff time in seconds (default: 15)
- `backoff_success_count` - Number of consecutive error-free checks before backoff increases (default: 5)

**[target_name]** - Each section defines a separate database target:
- `host` - Database server hostname or IP address
- `port` - Database server port (default: 3306)
- `username` - Database username with replication monitoring privileges
- `password` - Database password

You can add as many target sections as needed. All settings in the `[defaults]` section are optional; if omitted, the built-in defaults will be used.

## Usage

### Basic usage

```bash
./maria_repl_check
```

This will use `config.ini` in the current directory.

### Custom config file

```bash
./maria_repl_check -c /path/to/custom_config.ini
```

## Systemd Service Installation

To run the monitor as a systemd service:

1. **Install the binary and config:**
```bash
sudo mkdir -p /opt/daemons/myreplcheck
sudo cp maria_repl_check /opt/daemons/myreplcheck/
sudo cp config.ini.sample /opt/daemons/myreplcheck/config.ini
# Edit the config with your actual credentials
sudo nano /opt/daemons/myreplcheck/config.ini
sudo chmod +x /opt/daemons/myreplcheck/maria_repl_check
```

2. **Install the service file:**
```bash
sudo cp maria_repl_check.service /etc/systemd/system/
sudo systemctl daemon-reload
```

3. **Enable and start the service:**
```bash
sudo systemctl enable maria_repl_check
sudo systemctl start maria_repl_check
```

4. **Check status and logs:**
```bash
sudo systemctl status maria_repl_check
sudo journalctl -u maria_repl_check -f
```

The service will automatically restart on failure and start on system boot.

## Log Format

All log entries follow a structured format for easy parsing:

```
[target_name] YYYY/MM/DD HH:MM:SS errno=<code> <indicator> action=<action> backoff=<seconds>s
```

### Example logs

```
[ds092] 2025/12/17 12:03:03 errno=0 ✓ (caught up) action=none backoff=0s
[ds092] 2025/12/17 12:03:05 errno=1062 ↑ (catching up, lag: 1234 bytes) action=skip backoff=0s
[ds092] 2025/12/17 12:03:06 errno=0 → (stable, lag: 5678 bytes) action=none backoff=2s
[ds092] 2025/12/17 12:03:08 errno=0 lag: 158203659 bytes action=reset_position backoff=0s (Exec_Master_Log_Pos > Read_Master_Log_Pos)
```

### Log fields

- **errno** - MySQL/MariaDB error number (0 = no error)
- **indicator** - Replication progress indicator with lag information
- **action** - Action taken: `none`, `skip`, `optimize`, or `reset_position`
- **backoff** - Current backoff time in seconds

## Performance

The application is optimized for high performance:

- **Connection reuse** - Single persistent connection per target (unlike the original bash script)
- **Efficient column parsing** - Uses `sql.RawBytes` for unused columns
- **Batch SQL commands** - Multiple statements in single round-trip when possible
- **Minimal memory allocation** - Only parses required fields from `SHOW SLAVE STATUS`

Performance comparison with the original bash script:
- Significantly faster when actively fixing errors (no connection overhead)
- Comparable speed during normal monitoring
- Lower CPU usage with intelligent backoff

## Error Handling

### Automatic corrections

1. **Error 1062 (Duplicate key)**
   - Executes: `STOP SLAVE; SET GLOBAL sql_slave_skip_counter = 1; START SLAVE`
   - Skips the conflicting transaction

2. **Error 1942 (Parallel replication)**
   - Optimizes parallel replication settings
   - Adjusts thread count and queuing parameters

3. **Position mismatch**
   - Detects when `Exec_Master_Log_Pos > Read_Master_Log_Pos`
   - Executes: `STOP SLAVE; CHANGE MASTER TO master_log_pos=4; START SLAVE`
   - Resets replication to recover from interrupted state

### Connection resilience

- Automatic reconnection on connection loss
- Connection pool cycling on persistent errors
- Graceful handling of timeouts

## Backoff Strategy

The application uses intelligent backoff to reduce database load when idle:

- **Trigger**: 5 consecutive `errno=0` checks
- **Increment**: +1 second per check after trigger
- **Maximum**: 15 seconds
- **Reset conditions**:
  - Any replication error detected
  - Any corrective action taken
  - Significant lag increase (falling behind)

## Dependencies

- [github.com/go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) - MySQL driver
- [gopkg.in/ini.v1](https://gopkg.in/ini.v1) - INI file parser

## Architecture

- **Main goroutine** - Loads configuration and spawns monitor goroutines
- **Monitor goroutines** - One per target, independent operation
- **Connection pooling** - Single connection per target for optimal performance
- **Lag tracking** - Per-target state tracking for progress indicators

## Troubleshooting

### Connection timeouts

If you see frequent timeout errors, check:
- Network connectivity to the database server
- Database server load and responsiveness
- Firewall rules

The application automatically attempts reconnection.

### Slow performance

- Check `Seconds_Behind_Master` - high values indicate the slave is processing a backlog
- Monitor `backoff` values - high backoff means the system is idle (good)
- Review `action` frequency - frequent actions indicate active replication issues

### False "falling behind" indicators

This is normal when:
- Master is actively receiving writes
- Long-running transactions are being replicated
- Network latency varies

The backoff system handles this automatically.

## License

MIT License

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

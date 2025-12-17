package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/ini.v1"
)

type GlobalConfig struct {
	SlaveParallelMaxQueued     int
	SlaveParallelThreads       int
	SlaveDomainParallelThreads int
	MasterLogPos               int
	MaxBackoffSeconds          int
	BackoffSuccessCount        int
}

type Target struct {
	Name     string
	Host     string
	Port     string
	Username string
	Password string
}

type SlaveStatus struct {
	Errno               int
	ReadMasterLogPos    int64
	ExecMasterLogPos    int64
	SecondsBehindMaster int64
}

type LagTracker struct {
	LastReadPos        int64
	LastExecPos        int64
	LastCheck          time.Time
	ZeroErrCount       int
	BackoffSeconds     int
	LastSecondsBehind  int64
	CurrentErrorCode   int       // Track current error being handled
	ErrorCount         int       // Count of repeated errors
	FirstErrorTime     time.Time // When the current error series started
	LastLoggedErrorSeq int       // Track which error sequence was last logged
}

func loadConfig(filename string) (*GlobalConfig, []Target, error) {
	cfg, err := ini.Load(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config file: %w", err)
	}

	// Load global config with defaults
	globalCfg := &GlobalConfig{
		SlaveParallelMaxQueued:     262144,
		SlaveParallelThreads:       3,
		SlaveDomainParallelThreads: 2,
		MasterLogPos:               4,
		MaxBackoffSeconds:          15,
		BackoffSuccessCount:        5,
	}

	// Override with values from [defaults] section if present
	if cfg.HasSection("defaults") {
		defaults := cfg.Section("defaults")
		if defaults.HasKey("slave_parallel_max_queued") {
			globalCfg.SlaveParallelMaxQueued = defaults.Key("slave_parallel_max_queued").MustInt(262144)
		}
		if defaults.HasKey("slave_parallel_threads") {
			globalCfg.SlaveParallelThreads = defaults.Key("slave_parallel_threads").MustInt(3)
		}
		if defaults.HasKey("slave_domain_parallel_threads") {
			globalCfg.SlaveDomainParallelThreads = defaults.Key("slave_domain_parallel_threads").MustInt(2)
		}
		if defaults.HasKey("master_log_pos") {
			globalCfg.MasterLogPos = defaults.Key("master_log_pos").MustInt(4)
		}
		if defaults.HasKey("max_backoff_seconds") {
			globalCfg.MaxBackoffSeconds = defaults.Key("max_backoff_seconds").MustInt(15)
		}
		if defaults.HasKey("backoff_success_count") {
			globalCfg.BackoffSuccessCount = defaults.Key("backoff_success_count").MustInt(5)
		}
	}

	var targets []Target
	for _, section := range cfg.Sections() {
		// Skip the default section and defaults section
		if section.Name() == "DEFAULT" || section.Name() == "defaults" {
			continue
		}
		targets = append(targets, Target{
			Name:     section.Name(),
			Host:     section.Key("host").String(),
			Port:     section.Key("port").String(),
			Username: section.Key("username").String(),
			Password: section.Key("password").String(),
		})
	}

	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("no targets defined in config file")
	}

	return globalCfg, targets, nil
}

func main() {
	// Parse command line flags
	configFile := flag.String("c", "config.ini", "Path to config file")
	flag.Parse()

	// Load configuration
	globalCfg, targets, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Loaded %d target(s) from config\n", len(targets))

	// Start a goroutine for each target
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			monitorTarget(t, globalCfg)
		}(target)
	}

	// Wait for all goroutines to finish (they shouldn't unless there's a fatal error)
	wg.Wait()
}

func monitorTarget(target Target, cfg *GlobalConfig) {
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", target.Name), log.LstdFlags)

	// Create connection string with optimizations
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/?interpolateParams=true&timeout=10s&readTimeout=30s&writeTimeout=10s&multiStatements=true",
		target.Username, target.Password, target.Host, target.Port)

	// Open database connection (reused throughout)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		logger.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Configure connection pool - keep it lean with shorter lifetime
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	// Test connection
	if err := db.Ping(); err != nil {
		logger.Fatalf("Failed to ping database: %v", err)
	}

	logger.Println("Connected to MariaDB, monitoring replication status...")

	tracker := &LagTracker{
		LastCheck: time.Now(),
	}

	for {
		for {
			status, err := getSlaveStatus(db)
			if err != nil {
				logger.Printf("Error checking replication: %v", err)
				// Try to reconnect on connection errors
				if err := db.Ping(); err != nil {
					logger.Printf("Connection lost, reconnecting...")
				}
				time.Sleep(1 * time.Second)
				break
			}

			// Calculate lag indicator
			indicator := calculateLagIndicator(tracker, status)

			// Check for position mismatch (Exec > Read indicates replication interruption)
			if status.ExecMasterLogPos > status.ReadMasterLogPos && status.ExecMasterLogPos > 0 {
				logger.Printf("errno=%d %s action=reset_position backoff=%ds (Exec_Master_Log_Pos > Read_Master_Log_Pos)",
					status.Errno, indicator, tracker.BackoffSeconds)
				// Reset backoff when taking action
				tracker.ZeroErrCount = 0
				tracker.BackoffSeconds = 0
				if err := resetMasterLogPos(db, cfg); err != nil {
					logger.Printf("Failed to reset master log position: %v", err)
					if err := db.Ping(); err != nil {
						logger.Printf("Connection lost, will reconnect")
					}
					break
				}
				logger.Printf("Master log position reset to %d", cfg.MasterLogPos)
				// Continue to next iteration
				continue
			}

			var action string
			if status.Errno == 1062 {
				action = "action=skip"
				// Reset backoff when taking action
				tracker.ZeroErrCount = 0
				tracker.BackoffSeconds = 0

				// Track error sequence
				if tracker.CurrentErrorCode != 1062 {
					// New error type - log if previous sequence exists
					if tracker.ErrorCount > 1 {
						logger.Printf("Fixed %d more errno=%d problems", tracker.ErrorCount-1, tracker.CurrentErrorCode)
					}
					// Start new sequence
					tracker.CurrentErrorCode = 1062
					tracker.ErrorCount = 1
					tracker.FirstErrorTime = time.Now()
					logger.Printf("errno=%d %s %s backoff=%ds", status.Errno, indicator, action, tracker.BackoffSeconds)
				} else {
					// Same error continues
					tracker.ErrorCount++
				}

				if err := skipReplicationError(db); err != nil {
					logger.Printf("errno=%d %s %s backoff=%ds error=%v", status.Errno, indicator, action, tracker.BackoffSeconds, err)
					if err := db.Ping(); err != nil {
						logger.Printf("Connection lost, will reconnect")
					}
					break
				}
				// No sleep - loop immediately like bash script
			} else if status.Errno == 1942 {
				action = "action=optimize"
				// Reset backoff when taking action
				tracker.ZeroErrCount = 0
				tracker.BackoffSeconds = 0

				// Track error sequence
				if tracker.CurrentErrorCode != 1942 {
					// New error type - log if previous sequence exists
					if tracker.ErrorCount > 1 {
						logger.Printf("Fixed %d more errno=%d problems", tracker.ErrorCount-1, tracker.CurrentErrorCode)
					}
					// Start new sequence
					tracker.CurrentErrorCode = 1942
					tracker.ErrorCount = 1
					tracker.FirstErrorTime = time.Now()
					logger.Printf("errno=%d %s %s backoff=%ds", status.Errno, indicator, action, tracker.BackoffSeconds)
				} else {
					// Same error continues
					tracker.ErrorCount++
				}

				if err := optimizeReplication(db, cfg); err != nil {
					logger.Printf("errno=%d %s %s backoff=%ds error=%v", status.Errno, indicator, action, tracker.BackoffSeconds, err)
					if err := db.Ping(); err != nil {
						logger.Printf("Connection lost, will reconnect")
					}
					break
				}
				// No sleep - loop immediately
			} else {
				// Check if we just finished an error sequence
				if status.Errno == 0 && tracker.CurrentErrorCode != 0 {
					if tracker.ErrorCount > 1 {
						duration := time.Since(tracker.FirstErrorTime)
						logger.Printf("Fixed %d more errno=%d problems (took %v) %s", tracker.ErrorCount-1, tracker.CurrentErrorCode, duration.Round(time.Second), indicator)
					}
					// Reset error tracking
					tracker.CurrentErrorCode = 0
					tracker.ErrorCount = 0
				}

				// Only log if there's an error (errno != 0)
				if status.Errno != 0 {
					logger.Printf("errno=%d %s action=none backoff=%ds", status.Errno, indicator, tracker.BackoffSeconds)
				}

				// Backoff logic: only when errno=0 AND (caught up OR not falling further behind)
				if status.Errno == 0 {
					// Check if we're actually caught up or making progress
					isCaughtUp := status.SecondsBehindMaster == 0
					isProgressing := tracker.LastSecondsBehind > 0 && status.SecondsBehindMaster <= tracker.LastSecondsBehind

					if isCaughtUp || isProgressing {
						tracker.ZeroErrCount++
						if tracker.ZeroErrCount >= cfg.BackoffSuccessCount {
							// Increase backoff up to configured max
							tracker.BackoffSeconds++
							if tracker.BackoffSeconds > cfg.MaxBackoffSeconds {
								tracker.BackoffSeconds = cfg.MaxBackoffSeconds
							}
							// Only log at intervals (1s, 5s, 10s, 15s, etc.) or when reaching max
							if tracker.BackoffSeconds == 1 || tracker.BackoffSeconds%5 == 0 || tracker.BackoffSeconds == cfg.MaxBackoffSeconds {
								logger.Printf("No errors detected, backed off to %ds", tracker.BackoffSeconds)
							}
						}
					} else {
						// Falling behind, reset counter but keep current backoff
						tracker.ZeroErrCount = 0
					}
					tracker.LastSecondsBehind = status.SecondsBehindMaster
				} else {
					// Reset backoff on any error
					tracker.ZeroErrCount = 0
					if tracker.BackoffSeconds > 0 {
						logger.Printf("Error detected, resetting backoff")
						tracker.BackoffSeconds = 0
					}
				}
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		// Apply backoff if in backoff state, otherwise use default
		if tracker.BackoffSeconds > 0 {
			time.Sleep(time.Duration(tracker.BackoffSeconds) * time.Second)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func calculateLagIndicator(tracker *LagTracker, status *SlaveStatus) string {
	now := time.Now()
	elapsed := now.Sub(tracker.LastCheck).Seconds()

	// Calculate current lag
	currentLag := status.ReadMasterLogPos - status.ExecMasterLogPos

	// Skip rate calculation if this is the first check or too soon
	if tracker.LastReadPos == 0 || elapsed < 0.1 {
		tracker.LastReadPos = status.ReadMasterLogPos
		tracker.LastExecPos = status.ExecMasterLogPos
		tracker.LastCheck = now

		// Still show lag info even if we can't calculate rate yet
		if currentLag == 0 {
			return "✓ (caught up)"
		}
		return fmt.Sprintf("lag: %d bytes", currentLag)
	}

	// Calculate rates (bytes per second)
	readRate := float64(status.ReadMasterLogPos-tracker.LastReadPos) / elapsed
	execRate := float64(status.ExecMasterLogPos-tracker.LastExecPos) / elapsed

	// Update tracker
	tracker.LastReadPos = status.ReadMasterLogPos
	tracker.LastExecPos = status.ExecMasterLogPos
	tracker.LastCheck = now

	// Determine indicator
	var indicator string
	if currentLag == 0 {
		indicator = "✓ (caught up)"
	} else if execRate > readRate {
		// We're catching up
		indicator = fmt.Sprintf("↑ (catching up, lag: %d bytes)", currentLag)
	} else if execRate < readRate {
		// We're falling behind
		indicator = fmt.Sprintf("↓ (falling behind, lag: %d bytes)", currentLag)
	} else {
		// Rates are equal
		indicator = fmt.Sprintf("→ (stable, lag: %d bytes)", currentLag)
	}

	return indicator
}

func getSlaveStatus(db *sql.DB) (*SlaveStatus, error) {
	// Use raw query and parse only what we need - much faster than scanning all columns
	rows, err := db.Query("SHOW SLAVE STATUS")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// Find the indices of columns we need
	errnoIdx := -1
	readPosIdx := -1
	execPosIdx := -1
	secondsBehindIdx := -1
	for i, col := range columns {
		switch col {
		case "Last_SQL_Errno":
			errnoIdx = i
		case "Read_Master_Log_Pos":
			readPosIdx = i
		case "Exec_Master_Log_Pos":
			execPosIdx = i
		case "Seconds_Behind_Master":
			secondsBehindIdx = i
		}
	}

	if errnoIdx == -1 {
		return nil, fmt.Errorf("Last_SQL_Errno column not found")
	}

	if !rows.Next() {
		return &SlaveStatus{}, nil
	}

	// Create minimal scan targets - only allocate what we need
	values := make([]interface{}, len(columns))
	for i := range values {
		if i == errnoIdx || i == readPosIdx || i == execPosIdx || i == secondsBehindIdx {
			values[i] = new(sql.NullInt64)
		} else {
			values[i] = new(sql.RawBytes) // RawBytes is more efficient for unused columns
		}
	}

	if err := rows.Scan(values...); err != nil {
		return nil, err
	}

	status := &SlaveStatus{}

	if v, ok := values[errnoIdx].(*sql.NullInt64); ok && v.Valid {
		status.Errno = int(v.Int64)
	}

	if readPosIdx != -1 {
		if v, ok := values[readPosIdx].(*sql.NullInt64); ok && v.Valid {
			status.ReadMasterLogPos = v.Int64
		}
	}

	if execPosIdx != -1 {
		if v, ok := values[execPosIdx].(*sql.NullInt64); ok && v.Valid {
			status.ExecMasterLogPos = v.Int64
		}
	}

	if secondsBehindIdx != -1 {
		if v, ok := values[secondsBehindIdx].(*sql.NullInt64); ok && v.Valid {
			status.SecondsBehindMaster = v.Int64
		}
	}

	return status, nil
}

func skipReplicationError(db *sql.DB) error {
	// Execute all commands in one round-trip for speed
	_, err := db.Exec("STOP SLAVE; SET global sql_slave_skip_counter = 1; START SLAVE")
	return err
}

func resetMasterLogPos(db *sql.DB, cfg *GlobalConfig) error {
	// Reset master log position when Exec_Master_Log_Pos > Read_Master_Log_Pos
	query := fmt.Sprintf("STOP SLAVE; CHANGE MASTER TO master_log_pos=%d; START SLAVE", cfg.MasterLogPos)
	_, err := db.Exec(query)
	return err
}

func optimizeReplication(db *sql.DB, cfg *GlobalConfig) error {
	// Execute all commands in one round-trip for speed
	query := fmt.Sprintf("STOP SLAVE; SET GLOBAL slave_parallel_max_queued = %d; SET GLOBAL slave_parallel_threads = %d; SET GLOBAL slave_domain_parallel_threads = %d; SET GLOBAL slave_parallel_mode = 'optimistic'; START SLAVE;",
		cfg.SlaveParallelMaxQueued, cfg.SlaveParallelThreads, cfg.SlaveDomainParallelThreads)
	_, err := db.Exec(query)
	return err
}

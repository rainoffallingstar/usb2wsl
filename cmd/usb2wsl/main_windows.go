//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed ../../scripts/install-schtask.ps1
var embeddedInstallSchTaskPS1 string

type Config struct {
	// Required.
	WslDistro string `json:"wslDistro"`

	// Optional. If set, runs after each successful attach.
	// Executed as: wsl.exe -d <WslDistro> -- bash -lc <WslPostAttachBash>
	WslPostAttachBash string `json:"wslPostAttachBash"`

	// Optional. If true, ensure the distro is started (initialized) before polling/attaching.
	AutoStartDistro bool `json:"autoStartDistro"`

	// Device selection:
	// - If AllowVIDPID is non-empty, only devices whose VID:PID is in the list are considered.
	// - If AllowDeviceRegex is set, DEVICE column must match it (case-insensitive).
	AllowVIDPID      []string `json:"allowVIDPID"`
	AllowDeviceRegex string   `json:"allowDeviceRegex"`

	// Polling and commands.
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	UsbipdPath          string `json:"usbipdPath"` // default: usbipd
	WslExePath          string `json:"wslExePath"` // default: wsl.exe

	// Optional. If true and usbipd is missing, attempt to install it via winget.
	AutoInstallUsbipd bool `json:"autoInstallUsbipd"`

	// Optional. If true, open the mounted WSL path in Windows Explorer after a successful attach+mount.
	OpenExplorer          bool   `json:"openExplorer"`
	ExplorerPath          string `json:"explorerPath"`          // default: explorer.exe
	ExplorerOpenWslPath   string `json:"explorerOpenWslPath"`   // default: /mnt/usbipd
	ExplorerDebounceSeconds int  `json:"explorerDebounceSeconds"` // default: 5

	// Optional. If true, prompt before attaching devices that are not already allowed.
	// Prompt mapping (MessageBox):
	// - Yes: attach once
	// - No: skip
	// - Cancel: attach and remember (adds VID:PID to state file)
	PromptOnAttach bool `json:"promptOnAttach"`

	// Optional. State file for remembered VID:PID approvals. Default: usb2wsl.state.json next to the config.
	StatePath string `json:"statePath"`

	// Optional. Log file path. If set, logs go to both stdout and this file.
	LogPath string `json:"logPath"`
}

type UsbipdRow struct {
	BusID  string
	VIDPID string
	Device string
	State  string
}

type State struct {
	AllowVIDPID []string `json:"allowVIDPID"`
}

func main() {
	var (
		configPath = flag.String("config", "config.json", "Path to config JSON")
		logPath    = flag.String("log", "", "Optional log file path (overrides config if set)")
		once       = flag.Bool("once", false, "Run one scan/attach cycle then exit")
		verbose    = flag.Bool("v", false, "Verbose logs")
	)
	// Subcommands are handled before the main flagset parse.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "task":
			os.Exit(runTaskSubcommand(os.Args[2:]))
		}
	}
	flag.Parse()

	if runtime.GOOS != "windows" {
		log.Fatalf("this program is intended to run on Windows (GOOS=%s)", runtime.GOOS)
	}

	cfg, err := readConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.WslDistro == "" {
		log.Fatal("config: wslDistro is required")
	}
	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 2
	}
	if cfg.UsbipdPath == "" {
		cfg.UsbipdPath = "usbipd"
	}
	if cfg.WslExePath == "" {
		cfg.WslExePath = "wsl.exe"
	}
	if cfg.ExplorerPath == "" {
		cfg.ExplorerPath = "explorer.exe"
	}
	if cfg.ExplorerOpenWslPath == "" {
		cfg.ExplorerOpenWslPath = "/mnt/usbipd"
	}
	if cfg.ExplorerDebounceSeconds <= 0 {
		cfg.ExplorerDebounceSeconds = 5
	}
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(filepath.Dir(filepath.Clean(*configPath)), "usb2wsl.state.json")
	}
	if *logPath != "" {
		cfg.LogPath = *logPath
	}

	var deviceRe *regexp.Regexp
	if cfg.AllowDeviceRegex != "" {
		deviceRe, err = regexp.Compile("(?i)" + cfg.AllowDeviceRegex)
		if err != nil {
			log.Fatalf("config: invalid allowDeviceRegex: %v", err)
		}
	}

	allowVIDPID := map[string]struct{}{}
	for _, v := range cfg.AllowVIDPID {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		allowVIDPID[v] = struct{}{}
	}

	logger, closeLog, err := newLogger(cfg.LogPath)
	if err != nil {
		log.Fatal(err)
	}
	defer closeLog()
	if !*verbose {
		logger.SetFlags(log.LstdFlags)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := ensureWslReady(ctx, cfg.WslExePath, cfg.WslDistro, cfg.AutoStartDistro); err != nil {
		logger.Fatal(err)
	}
	if err := ensureUsbipdReady(ctx, cfg.UsbipdPath, cfg.AutoInstallUsbipd, logger); err != nil {
		logger.Fatal(err)
	}

	state, err := loadState(cfg.StatePath)
	if err != nil {
		logger.Printf("state load failed (%s): %v", cfg.StatePath, err)
	}
	for _, v := range state.AllowVIDPID {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v != "" {
			allowVIDPID[v] = struct{}{}
		}
	}
	var stateMu sync.Mutex

	attached := map[string]time.Time{} // busid -> last attach
	lastExplorerOpen := time.Time{}
	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	runCycle := func() {
		rows, err := usbipdList(ctx, cfg.UsbipdPath)
		if err != nil {
			logger.Printf("usbipd list failed: %v", err)
			return
		}

		for _, row := range rows {
			if row.BusID == "" || row.VIDPID == "" {
				continue
			}
			if deviceRe != nil && !deviceRe.MatchString(row.Device) {
				continue
			}

			rowState := strings.ToLower(row.State)
			if strings.Contains(rowState, "attached") {
				continue
			}

			vidpidUpper := strings.ToUpper(row.VIDPID)
			_, isAllowed := allowVIDPID[vidpidUpper]
			rememberOnSuccess := false
			if !isAllowed && cfg.PromptOnAttach {
				choice, err := promptAttach(row)
				if err != nil {
					logger.Printf("prompt failed: %v", err)
					continue
				}
				switch choice {
				case promptSkip:
					continue
				case promptAttachOnce:
					// proceed
				case promptAttachRemember:
					rememberOnSuccess = true
				}
			} else if !isAllowed && len(allowVIDPID) > 0 {
				// In allow-list mode without prompt, skip unknown devices.
				continue
			}

			// Avoid hammering the same BUSID if attach fails repeatedly.
			if last, ok := attached[row.BusID]; ok && time.Since(last) < 10*time.Second {
				continue
			}
			attached[row.BusID] = time.Now()

			logger.Printf("attaching BUSID=%s VIDPID=%s DEVICE=%q STATE=%q", row.BusID, row.VIDPID, row.Device, row.State)
			if err := usbipdBind(ctx, cfg.UsbipdPath, row.BusID); err != nil {
				logger.Printf("usbipd bind %s failed: %v", row.BusID, err)
				continue
			}
			if err := usbipdAttachWSL(ctx, cfg.UsbipdPath, row.BusID); err != nil {
				logger.Printf("usbipd attach %s failed: %v", row.BusID, err)
				continue
			}
				mountedPaths := []string{}
				if cfg.WslPostAttachBash != "" {
					out, err := wslBashOut(ctx, cfg.WslExePath, cfg.WslDistro, cfg.WslPostAttachBash)
					if err != nil {
						logger.Printf("wsl post-attach failed: %v", err)
					} else {
						mountedPaths = parseMountedPaths(out)
					}
				}
			if rememberOnSuccess {
				stateMu.Lock()
				if _, ok := allowVIDPID[vidpidUpper]; !ok {
					allowVIDPID[vidpidUpper] = struct{}{}
					state.AllowVIDPID = append(state.AllowVIDPID, vidpidUpper)
					if err := saveState(cfg.StatePath, state); err != nil {
						logger.Printf("state save failed: %v", err)
					} else {
						logger.Printf("remembered VID:PID %s in %s", vidpidUpper, cfg.StatePath)
					}
				}
				stateMu.Unlock()
			}
				if cfg.OpenExplorer {
					pathsToOpen := mountedPaths
					if len(pathsToOpen) == 0 {
						pathsToOpen = []string{cfg.ExplorerOpenWslPath}
					}
					if time.Since(lastExplorerOpen) >= time.Duration(cfg.ExplorerDebounceSeconds)*time.Second {
						if err := openExplorerToWslPaths(ctx, cfg.ExplorerPath, cfg.WslDistro, pathsToOpen); err != nil {
							logger.Printf("open explorer failed: %v", err)
						} else {
							lastExplorerOpen = time.Now()
						}
					}
				}
		}
	}

	runCycle()
	if *once {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCycle()
		}
	}
}

func runTaskSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: usb2wsl task install [options]")
		return 2
	}
	switch args[0] {
	case "install":
		return runTaskInstall(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		return 2
	}
}

func runTaskInstall(args []string) int {
	fs := flag.NewFlagSet("task install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	exePath := fs.String("exe", "", "Path to usb2wsl.exe (default: current executable)")
	configPath := fs.String("config", "", "Path to config.json (default: <exe dir>\\config.json)")
	taskName := fs.String("name", "usb2wsl", "Scheduled task name")
	trigger := fs.String("trigger", "ONLOGON", "Trigger: ONLOGON or ONSTART")
	delaySeconds := fs.Int("delay", 15, "Delay before start (seconds)")
	restartCount := fs.Int("retries", 3, "Restart attempts on failure")
	restartIntervalSeconds := fs.Int("retryDelay", 10, "Restart interval (seconds)")
	executionTimeLimitMinutes := fs.Int("timeLimitMin", 0, "0 means no limit")
	workingDirectory := fs.String("workdir", "", "Working directory (default: exe directory)")
	logPath := fs.String("log", "", "Optional log file passed to usb2wsl.exe -log")

	onStart := fs.Bool("onstart", false, "Shortcut for -trigger ONSTART")
	onLogon := fs.Bool("onlogon", false, "Shortcut for -trigger ONLOGON")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *exePath == "" {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get executable path: %v\n", err)
			return 1
		}
		*exePath = self
	}
	exeDir := filepath.Dir(filepath.Clean(*exePath))
	if *workingDirectory == "" {
		*workingDirectory = exeDir
	}
	if *configPath == "" {
		*configPath = filepath.Join(exeDir, "config.json")
	}

	if *onStart && *onLogon {
		fmt.Fprintln(os.Stderr, "only one of -onstart / -onlogon can be set")
		return 2
	}
	if *onStart {
		*trigger = "ONSTART"
	}
	if *onLogon {
		*trigger = "ONLOGON"
	}

	psArgs := []string{
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-Command", embeddedInstallSchTaskPS1,
		"-ExePath", *exePath,
		"-ConfigPath", *configPath,
		"-TaskName", *taskName,
		"-Trigger", *trigger,
		"-DelaySeconds", fmt.Sprint(*delaySeconds),
		"-RestartCount", fmt.Sprint(*restartCount),
		"-RestartIntervalSeconds", fmt.Sprint(*restartIntervalSeconds),
		"-ExecutionTimeLimitMinutes", fmt.Sprint(*executionTimeLimitMinutes),
	}
	psArgs = append(psArgs, "-WorkingDirectory", *workingDirectory)
	if *logPath != "" {
		psArgs = append(psArgs, "-LogPath", *logPath)
	}

	cmd := exec.Command("powershell.exe", psArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "task install failed: %v\n", err)
		return 1
	}
	return 0
}

func newLogger(path string) (*log.Logger, func(), error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stdout, "", log.LstdFlags), func() {}, nil
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}, err
	}
	return log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags), func() { _ = f.Close() }, nil
}

func readConfig(path string) (Config, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func usbipdList(ctx context.Context, usbipd string) ([]UsbipdRow, error) {
	out, err := execCmd(ctx, usbipd, "list")
	if err != nil {
		return nil, err
	}
	return parseUsbipdList(bytes.NewReader(out))
}

func usbipdBind(ctx context.Context, usbipd, busid string) error {
	_, err := execCmd(ctx, usbipd, "bind", "--busid", busid)
	return err
}

func usbipdAttachWSL(ctx context.Context, usbipd, busid string) error {
	_, err := execCmd(ctx, usbipd, "attach", "--wsl", "--busid", busid)
	return err
}

func wslBashOut(ctx context.Context, wslExe, distro, bashCmd string) (string, error) {
	out, err := execCmd(ctx, wslExe, "-d", distro, "--", "bash", "-lc", bashCmd)
	return string(out), err
}

func execCmd(ctx context.Context, exe string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	if msg == "" {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s", err, msg)
}

func ensureWslReady(ctx context.Context, wslExe, distro string, autoStart bool) error {
	if _, err := exec.LookPath(wslExe); err != nil {
		return fmt.Errorf("wsl not found (%s): %w", wslExe, err)
	}
	out, err := execCmd(ctx, wslExe, "-l", "-v")
	if err != nil {
		return fmt.Errorf("wsl list failed: %w", err)
	}
	if !strings.Contains(string(out), distro) {
		return fmt.Errorf("wsl distro %q not found (check `wsl -l -v` output)", distro)
	}
	if autoStart {
		// Runs a trivial command to start/initialize the distro if it is stopped.
		if _, err := wslBashOut(ctx, wslExe, distro, "true"); err != nil {
			return fmt.Errorf("failed to start wsl distro %q: %w", distro, err)
		}
	}
	return nil
}

func ensureUsbipdReady(ctx context.Context, usbipd string, autoInstall bool, logger *log.Logger) error {
	if _, err := exec.LookPath(usbipd); err == nil {
		return nil
	}
	if !autoInstall {
		return fmt.Errorf("usbipd not found (%s). Install usbipd-win, or set autoInstallUsbipd=true", usbipd)
	}
	if _, err := exec.LookPath("winget"); err != nil {
		return fmt.Errorf("usbipd not found and winget is unavailable; install usbipd-win manually")
	}
	logger.Printf("usbipd not found; attempting install via winget")
	id, err := wingetFindUsbipdID(ctx)
	if err != nil {
		return fmt.Errorf("failed to find usbipd-win via winget: %w", err)
	}
	_, err = execCmd(ctx, "winget", "install", "--id", id, "-e", "--accept-package-agreements", "--accept-source-agreements")
	if err != nil {
		return fmt.Errorf("winget install failed for id=%q: %w", id, err)
	}
	if _, err := exec.LookPath(usbipd); err != nil {
		return fmt.Errorf("usbipd still not found after install; reopen terminal or check PATH")
	}
	return nil
}

func wingetFindUsbipdID(ctx context.Context) (string, error) {
	// Use a broad query and then pick the first row whose Name or Id mentions usbipd.
	out, err := execCmd(ctx, "winget", "search", "usbipd")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	// winget prints a table. We'll scan lines for a token that looks like an Id containing "usbipd".
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "Name") || strings.HasPrefix(l, "-") {
			continue
		}
		if !strings.Contains(strings.ToLower(l), "usbipd") {
			continue
		}
		fields := strings.Fields(l)
		// Try to find a field with a dot-separated Id (Publisher.App).
		for _, f := range fields {
			if strings.Contains(f, ".") && strings.Contains(strings.ToLower(f), "usbipd") {
				return f, nil
			}
		}
		// Fallback: second column is often Id.
		if len(fields) >= 2 && strings.Contains(strings.ToLower(fields[1]), "usbipd") {
			return fields[1], nil
		}
	}
	return "", errors.New("no matching winget package id found")
}

func openExplorerToWslPaths(ctx context.Context, explorerExe, distro string, wslPaths []string) error {
	if _, err := exec.LookPath(explorerExe); err != nil {
		return fmt.Errorf("explorer not found (%s): %w", explorerExe, err)
	}
	for _, wslPath := range wslPaths {
		p := strings.TrimSpace(wslPath)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		unc := `\\wsl$\` + distro + strings.ReplaceAll(p, "/", `\`)
		cmd := exec.CommandContext(ctx, explorerExe, unc)
		_ = cmd.Start()
	}
	return nil
}

func parseMountedPaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			paths = append(paths, p)
		}
	}
	return uniqueStrings(paths)
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func loadState(path string) (State, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

func saveState(path string, st State) error {
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type promptChoice int

const (
	promptAttachOnce promptChoice = iota
	promptSkip
	promptAttachRemember
)

func promptAttach(row UsbipdRow) (promptChoice, error) {
	title := "usb2wsl"
	text := fmt.Sprintf("检测到 USB 设备：\n\nBUSID: %s\nVID:PID: %s\nDEVICE: %s\n\n是否转接到 WSL？\n\n是(Y)：转接一次\n否(N)：不转接\n取消：转接并记住该 VID:PID", row.BusID, row.VIDPID, row.Device)
	// MB_YESNOCANCEL = 0x00000003, MB_ICONQUESTION = 0x00000020, MB_SYSTEMMODAL = 0x00001000
	ret, err := messageBox(title, text, 0x00000003|0x00000020|0x00001000)
	if err != nil {
		return promptSkip, err
	}
	switch ret {
	case 6: // IDYES
		return promptAttachOnce, nil
	case 7: // IDNO
		return promptSkip, nil
	case 2: // IDCANCEL
		return promptAttachRemember, nil
	default:
		return promptSkip, nil
	}
}

func messageBox(title, text string, flags uintptr) (int32, error) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	tPtr, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return 0, err
	}
	cPtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return 0, err
	}
	r, _, callErr := proc.Call(0, uintptr(unsafe.Pointer(tPtr)), uintptr(unsafe.Pointer(cPtr)), flags)
	if r == 0 {
		if callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, errors.New("MessageBoxW failed")
	}
	return int32(r), nil
}

func parseUsbipdList(r io.Reader) ([]UsbipdRow, error) {
	// Typical output:
	// BUSID  VID:PID    DEVICE                                                        STATE
	// 1-2    1234:5678  Foo USB Mass Storage Device                                     Not shared
	// 1-3    abcd:ef01  USB External Optical Drive                                      Not shared
	//
	// We parse by fixed columns using header indices if present; otherwise fall back to a conservative split.
	sc := bufio.NewScanner(r)
	var lines []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, errors.New("empty output")
	}

	header := strings.ToUpper(lines[0])
	busIdx := strings.Index(header, "BUSID")
	vidIdx := strings.Index(header, "VID:PID")
	devIdx := strings.Index(header, "DEVICE")
	stateIdx := strings.Index(header, "STATE")
	hasHeader := busIdx >= 0 && vidIdx >= 0 && devIdx >= 0 && stateIdx >= 0

	var rows []UsbipdRow
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, " ")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if hasHeader && len(line) > stateIdx {
			row := UsbipdRow{
				BusID:  strings.TrimSpace(sliceSafe(line, busIdx, vidIdx)),
				VIDPID: strings.TrimSpace(sliceSafe(line, vidIdx, devIdx)),
				Device: strings.TrimSpace(sliceSafe(line, devIdx, stateIdx)),
				State:  strings.TrimSpace(sliceSafe(line, stateIdx, len(line))),
			}
			// Some versions have a leading "Persisted:" line or other noise; ignore rows without BUSID.
			if row.BusID == "" || !strings.Contains(row.BusID, "-") {
				continue
			}
			rows = append(rows, row)
			continue
		}

		// Fallback: split into at most 4 fields; DEVICE may have spaces, so treat last two as DEVICE+STATE
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		busid := parts[0]
		vidpid := parts[1]
		state := strings.Join(parts[len(parts)-2:], " ")
		device := strings.Join(parts[2:len(parts)-2], " ")
		if busid == "" || !strings.Contains(busid, "-") {
			continue
		}
		rows = append(rows, UsbipdRow{BusID: busid, VIDPID: vidpid, Device: device, State: state})
	}
	return rows, nil
}

func sliceSafe(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if start > len(s) {
		start = len(s)
	}
	if end > len(s) {
		end = len(s)
	}
	if start > end {
		start = end
	}
	return s[start:end]
}

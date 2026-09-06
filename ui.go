package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// color system

type colorType string

const (
	colorInput     colorType = "input"
	colorSuccess   colorType = "success"
	colorInfo      colorType = "info"
	colorWarning   colorType = "warning"
	colorError     colorType = "error"
	colorException colorType = "exception"
)

var colorCodes = map[colorType][3]int{
	colorInput:     {0, 200, 200},
	colorSuccess:   {80, 220, 80},
	colorInfo:      {255, 175, 50},
	colorWarning:   {255, 220, 60},
	colorError:     {255, 90, 90},
	colorException: {220, 120, 255},
}

var isTerminal = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}()

func colorText(textType colorType, text string) string {
	if !isTerminal || (cfg != nil && cfg.noColor) {
		return text
	}
	c, ok := colorCodes[textType]
	if !ok {
		return text
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%dm%s\033[0m", c[0], c[1], c[2], text)
}

func colorStart(textType colorType) string {
	if !isTerminal || (cfg != nil && cfg.noColor) {
		return ""
	}
	c, ok := colorCodes[textType]
	if !ok {
		return ""
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", c[0], c[1], c[2])
}

// terminal width

func getTerminalWidth() int {
	if !isTerminal {
		return 80
	}
	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	ws := winsize{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(os.Stdout.Fd()), uintptr(0x5413), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

// output serialization

var outputMu sync.Mutex

// logging

func logMsg(level, format string, args ...interface{}) {
	outputMu.Lock()
	defer outputMu.Unlock()

	_clearProgressLine()

	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(os.Stdout, "%s %s %s\n", ts, colorText(colorType(strings.ToLower(level)), fmt.Sprintf("%-7s", level)), msg)
}

func infof(format string, args ...interface{})  { logMsg("INFO", format, args...) }
func errorf(format string, args ...interface{}) { logMsg("ERROR", format, args...) }
func warningf(format string, args ...interface{}) {
	logMsg("WARNING", format, args...)
}
func debugf(format string, args ...interface{}) {
	if cfg.debug {
		logMsg("DEBUG", format, args...)
	}
}

// status messages

func statusMsg(text string) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_clearProgressLine()
	fmt.Printf("%s... ", text)
	fmt.Println(colorText(colorSuccess, "Done"))
}

// download line

func downloadLine(counter int, name string, size float64) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_clearProgressLine()
	if size > 0 {
		fmt.Printf("Get:%-3d %s  [%s]\n", counter, colorText(colorInfo, name), formatSize(size))
	} else {
		fmt.Printf("Get:%-3d %s\n", counter, colorText(colorInfo, name))
	}
}

// fetch summary

func fetchSummary(totalBytes float64, elapsed time.Duration) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_clearProgressLine()
	speed := float64(0)
	if elapsed.Seconds() > 0 {
		speed = totalBytes / elapsed.Seconds()
	}
	fmt.Printf("Fetched %s in %s", formatSize(totalBytes), formatDuration(elapsed))
	if speed > 0 {
		fmt.Printf(" (%s/s)", formatSpeed(speed))
	}
	fmt.Println()
}

func formatSpeed(bytesPerSec float64) string {
	const KB = 1000
	const MB = 1000 * KB
	switch {
	case bytesPerSec >= MB:
		return fmt.Sprintf("%.2f MB/s", bytesPerSec/MB)
	case bytesPerSec >= KB:
		return fmt.Sprintf("%.2f kB/s", bytesPerSec/KB)
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

// size formatting

func formatSize(bytes float64) string {
	const (
		KiB = 1000
		MiB = 1000 * KiB
		GiB = 1000 * MiB
	)
	switch {
	case bytes >= GiB:
		return fmt.Sprintf("%.2f GiB", bytes/GiB)
	case bytes >= MiB:
		return fmt.Sprintf("%.2f MiB", bytes/MiB)
	case bytes >= KiB:
		return fmt.Sprintf("%.2f KiB", bytes/KiB)
	default:
		return fmt.Sprintf("%.0f B", bytes)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d >= time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		s := int(d.Seconds()) % 60
		if h >= 100 {
			return fmt.Sprintf("%dh%02dm", h, m)
		}
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// progress tracking

type progressState struct {
	mu         sync.Mutex
	total      int
	completed  int
	startTime  time.Time
	drawn      bool
	spinnerIdx int
	stop       chan struct{}
}

var prog *progressState

var spinnerChars = [4]byte{'|', '/', '-', '\\'}

func newProgressState(total int) *progressState {
	return &progressState{
		total:     total,
		startTime: time.Now(),
		stop:      make(chan struct{}),
	}
}

func (p *progressState) increment() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed < p.total {
		p.completed++
	}
}

func (p *progressState) startTicker() {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				outputMu.Lock()
				_drawProgressLine()
				outputMu.Unlock()
			}
		}
	}()
}

func (p *progressState) stopTicker() {
	close(p.stop)
}

func _clearProgressLine() {
	if prog == nil || !prog.drawn || !isTerminal || cfg.noColor {
		return
	}
	fmt.Print("\r\033[K")
	prog.drawn = false
}

func _drawProgressLine() {
	if prog == nil || !isTerminal || cfg.noColor || prog.total == 0 {
		return
	}

	prog.mu.Lock()
	completed := prog.completed
	total := prog.total
	startTime := prog.startTime
	prog.mu.Unlock()

	// Refresh terminal width each frame so a live resize never wraps the line.
	width := getTerminalWidth()

	prog.spinnerIdx++

	fmt.Print("\r\033[K" + buildProgressLine(width, completed, total, time.Since(startTime), prog.spinnerIdx))
	prog.drawn = true
}

func buildProgressLine(width, completed, total int, elapsed time.Duration, spinnerIdx int) string {
	percent := 0
	if total > 0 {
		percent = int(float64(completed) / float64(total) * 100)
	}

	// overall bar: "Progress: [ NN%] [####.....]" gracefully stolen from APT
	prefix := fmt.Sprintf("Progress: [%3d%%] [%c ", percent, spinnerChars[spinnerIdx%len(spinnerChars)])

	suffix := fmt.Sprintf("] %d/%d", completed, total)

	if completed > 0 && elapsed.Seconds() > 1 {
		rate := float64(completed) / elapsed.Seconds()
		remaining := float64(total-completed) / rate
		suffix += fmt.Sprintf(" %s", formatDuration(time.Duration(remaining*float64(time.Second))))
	}

	barW := width - len(prefix) - len(suffix)
	if barW < 1 {
		line := prefix + suffix
		if len(line) > width {
			line = line[:width]
		}
		return line
	}

	filled := int(float64(barW) * float64(percent) / 100)
	if filled > barW {
		filled = barW
	}
	bar := strings.Repeat("#", filled) + strings.Repeat(".", barW-filled)

	return prefix + colorText(colorInfo, bar) + suffix
}

func (p *progressState) finish() {
	p.stopTicker()
	outputMu.Lock()
	defer outputMu.Unlock()
	_clearProgressLine()
	fmt.Println()
}

// transaction summary

type packageInfo struct {
	Name    string
	Edition string
	Arch    string
	Action  string
	Size    float64
}

func (p packageInfo) String() string {
	return fmt.Sprintf("%s-%s.%s", p.Name, p.Edition, p.Arch)
}

func printTransactionSummary(installs, removes []packageInfo, downloadSize, diffBytes float64, numPkgs int) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_clearProgressLine()

	fmt.Println()

	// Count upgrades vs new installs
	nUpgrades := 0
	nInstalls := 0
	for _, pkg := range installs {
		if pkg.Edition != "" && pkg.Arch != "" {
			nUpgrades++
		} else {
			nInstalls++
		}
	}
	nRemoves := len(removes)

	// Packages to download (upgrades)
	if nUpgrades > 0 {
		names := make([]string, nUpgrades)
		for i, pkg := range installs {
			if i >= nUpgrades {
				break
			}
			names[i] = colorText(colorInfo, pkg.String())
		}
		fmt.Printf("The following packages will be upgraded:\n")
		printWrappedNames(names, getTerminalWidth(), "  ")
		fmt.Println()
	}

	// New installs
	if nInstalls > 0 {
		names := make([]string, nInstalls)
		for i := nUpgrades; i < len(installs) && i-nUpgrades < nInstalls; i++ {
			names[i-nUpgrades] = colorText(colorInfo, installs[i].String())
		}
		fmt.Printf("The following NEW packages will be installed:\n")
		printWrappedNames(names, getTerminalWidth(), "  ")
		fmt.Println()
	}

	// Removed packages
	if nRemoves > 0 {
		names := make([]string, nRemoves)
		for i, pkg := range removes {
			names[i] = colorText(colorError, pkg.String())
		}
		fmt.Printf("The following packages will be REMOVED:\n")
		printWrappedNames(names, getTerminalWidth(), "  ")
		fmt.Println()
	}

	// Summary line
	parts := []string{}
	if nUpgrades > 0 {
		parts = append(parts, fmt.Sprintf("%d upgraded", nUpgrades))
	}
	if nInstalls > 0 {
		parts = append(parts, fmt.Sprintf("%d newly installed", nInstalls))
	}
	if nRemoves > 0 {
		parts = append(parts, fmt.Sprintf("%d to remove", nRemoves))
	}
	parts = append(parts, fmt.Sprintf("%d not upgraded", 0))
	fmt.Printf("%s.\n", strings.Join(parts, ", "))

	// Download and space info
	if downloadSize > 0 {
		fmt.Printf("Need to get %s of archives.\n", formatSize(downloadSize))
	}

	spacePrefix := ""
	if diffBytes >= 0 {
		spacePrefix = "+"
	}
	fmt.Printf("After this operation, %s%s of additional disk space will be used.\n",
		spacePrefix, formatSize(diffBytes))
}

// printWrappedNames prints package names wrapped to fit within termWidth.
func printWrappedNames(names []string, termWidth int, indent string) {
	lineLen := len(indent)
	first := true
	for _, name := range names {
		nameLen := len(name)
		if !first && lineLen+1+nameLen > termWidth-2 {
			fmt.Println()
			lineLen = len(indent)
			first = true
		}
		if !first {
			fmt.Print(" ")
			lineLen++
		}
		fmt.Printf("%s%s", indent, name)
		if first {
			lineLen += nameLen
			first = false
		} else {
			lineLen += nameLen
		}
	}
	fmt.Println()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

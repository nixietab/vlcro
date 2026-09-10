package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

const (
	vlcroVersion  = "0.1.0"
	zypperPidFile = "/run/zypp.pid"
	maxRetries    = 3
	retryDelay    = 100_000_000 // 100ms as int64 nanoseconds
)

var cfg *config

type config struct {
	version      bool
	noConfirm    bool
	jobs         int
	debug        bool
	noColor      bool
	command      string
	force        bool
	downloadOnly bool
	packages     []string
	txInfo       []packageInfo // parsed packages from dry-run
}

// CLI

func printHelp() {
	// Ensure cfg is available for colorStart
	if cfg == nil {
		cfg = &config{jobs: 10}
	}

	// highlight colors
	hi := colorStart(colorInfo)    // section headers / "vlcro"
	hc := colorStart(colorWarning) // usage keyword
	he := colorStart(colorError)   // options
	hd := colorStart(colorInput)   // command bold-ish
	d := "\033[0m"
	if !isTerminal || cfg.noColor {
		d = ""
	}

	usage := fmt.Sprintf(`%susage%s: vlcro [-h] [-v] [-y] [-j N] [--debug] [--no-color]
%s      {refresh,ref,dist-upgrade,dup,update,up,install,in,install-new-recommends,inr,search,se} ...

%s%s%s (%sv%s%s) makes zypper faster by running slow operations in parallel.

%soptions%s:
  %s-h, --help%s            show this help message and exit
  %s-v, -V, --version%s     print version number and exit
  %s-y, --no-confirm%s      automatic yes to prompts, run non-interactively
  %s-j, --jobs%s            number of parallel operations (1-64, default: 10)
  %s--debug%s               enable debug output
  %s--no-color%s            disable color output

%scommands%s:
  %srefresh%s (%sref%s)         refresh all enabled repos
    %s-f, --force%s         force a complete refresh

  %sdist-upgrade%s (%sdup%s)   perform distribution upgrade
    %s-d, --download-only%s download packages without installing

  %supdate%s (%sup%s)         update all installed packages
    %s-d, --download-only%s download packages without installing

  %ssearch%s (%sse%s)          search for packages matching pattern
    %s<pattern>%s           pattern(s) to search for

  %sinstall%s (%sin%s)         install one or more packages
    %s-d, --download-only%s download packages without installing
    %s<package>%s           package name(s) to install

  %sinstall-new-recommends%s (%sinr%s)
                        install new packages recommended by already installed ones
    %s-d, --download-only%s download packages without installing
`,
		hc, d, hc,
		hi, "vlcro", d, hi, vlcroVersion, d,
		hi, d,
		he, d, he, d, he, d, he, d, he, d, he, d,
		hi, d,
		hd, d, hd, d, he, d,
		hd, d, hd, d, he, d,
		hd, d, hd, d, he, d,
		hd, d, hd, d, hd, d,
		hd, d, hd, d, he, d, hd, d,
		hd, d, hd, d, he, d)

	fmt.Print(usage)
}

func parseArgs() *config {
	c, err := parseArgsInto(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return c
}

func parseJobs(c *config, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("Invalid jobs value: %s", value)
	}
	if n < 1 || n > 64 {
		return fmt.Errorf("Invalid jobs value: %d. Must be between 1 and 64", n)
	}
	c.jobs = n
	return nil
}

func isRefreshCommand(cmd string) bool {
	return cmd == "refresh" || cmd == "ref"
}

func isInstallCommand(cmd string) bool {
	return cmd == "install" || cmd == "in"
}

func isSearchCommand(cmd string) bool {
	return cmd == "search" || cmd == "se"
}

func isKnownCommand(cmd string) bool {
	switch cmd {
	case "refresh", "ref", "dist-upgrade", "dup", "update", "up",
		"install", "in", "install-new-recommends", "inr", "search", "se":
		return true
	}
	return false
}

func parseArgsInto(args []string) (*config, error) {
	c := &config{jobs: 10}
	i := 0
	cmdFound := false
	packagesOnly := false

	for i < len(args) {
		arg := args[i]

		// search/se passes all remaining patterns and zypper search
		// options verbatim to zypper.
		if cmdFound && isSearchCommand(c.command) {
			c.packages = append(c.packages, arg)
			i++
			continue
		}

		consumedNext := false

		switch {
		case arg == "-h" || arg == "--help":
			printHelp()
			os.Exit(0)
		case arg == "-v" || arg == "-V" || arg == "--version":
			c.version = true
			i++
			continue
		case arg == "-y" || arg == "--no-confirm":
			c.noConfirm = true
			i++
			continue
		case arg == "--debug":
			c.debug = true
			i++
			continue
		case arg == "--no-color":
			c.noColor = true
			i++
			continue
		case arg == "-f" || arg == "--force":
			c.force = true
			i++
			continue
		case arg == "-d" || arg == "--download-only":
			c.downloadOnly = true
			i++
			continue
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg, "=")
			if name == "--jobs" {
				if !hasValue {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("Error: --jobs requires an argument")
					}
					value = args[i+1]
					i += 2
				} else {
					i++
				}
				if err := parseJobs(c, value); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("Unknown option: %s", arg)
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			body := arg[1:]
			for k := 0; k < len(body); k++ {
				switch body[k] {
				case 'h':
					printHelp()
					os.Exit(0)
				case 'v', 'V':
					c.version = true
				case 'y':
					c.noConfirm = true
				case 'f':
					c.force = true
				case 'd':
					c.downloadOnly = true
				case 'j':
					rest := body[k+1:]
					if strings.HasPrefix(rest, "=") {
						rest = rest[1:]
					}
					if rest == "" {
						if i+1 >= len(args) {
							return nil, fmt.Errorf("Error: --jobs requires an argument")
						}
						rest = args[i+1]
						consumedNext = true
					}
					if err := parseJobs(c, rest); err != nil {
						return nil, err
					}
					k = len(body)
				default:
					return nil, fmt.Errorf("Unknown option: -%s", string(body[k]))
				}
			}
			if !consumedNext {
				i++
			} else {
				i += 2
			}
			continue
		default:
			if !cmdFound {
				c.command = arg
				cmdFound = true
				packagesOnly = isInstallCommand(arg)
				i++
				continue
			}
			if !packagesOnly {
				return nil, fmt.Errorf("Unexpected argument: %s", arg)
			}
			c.packages = append(c.packages, arg)
			i++
			continue
		}
	}

	if !cmdFound {
		return c, nil
	}

	if !isKnownCommand(c.command) {
		return nil, fmt.Errorf("Unknown command: %s", c.command)
	}
	if c.force && !isRefreshCommand(c.command) {
		return nil, fmt.Errorf("Option --force is only valid with the refresh command")
	}
	if c.downloadOnly && isRefreshCommand(c.command) {
		return nil, fmt.Errorf("Option --download-only is not valid with the refresh command")
	}

	return c, nil
}

// dependency check (exec.LookPath)

func checkDependencies() {
	required := []string{"zypper", "findmnt", "bash"}
	missing := []string{}
	for _, program := range required {
		if _, err := exec.LookPath(program); err != nil {
			missing = append(missing, program)
		}
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("Bailing out, missing required dependencies: %s\n"+
			"The following are required for vlcro to function: %s",
			strings.Join(missing, ", "),
			strings.Join(required, ", "))
		errorf("%s", colorText(colorError, msg))
		os.Exit(4)
	}
}

// main

func main() {
	cfg = parseArgs()

	if cfg.version {
		fmt.Printf("vlcro v%s\n", vlcroVersion)
		return
	}
	if cfg.command == "" {
		printHelp()
		return
	}

	if os.Getuid() != 0 {
		errorf("%s", colorText(colorError, "Bailing out, program must be run with root privileges"))
		os.Exit(3)
	}

	checkDependencies()

	if pid, program := checkZypperRunning(); pid != 0 {
		msg := fmt.Sprintf("zypper is already invoked by the application with pid %d (%s).\nClose this application before trying again.", pid, program)
		errorf("%s", colorText(colorError, msg))
		os.Exit(5)
	}

	tmpDir, err := os.MkdirTemp("/tmp", "vlcro_")
	if err != nil {
		errorf("%s", colorText(colorError, fmt.Sprintf("Failed to create temp directory: %v", err)))
		os.Exit(1)
	}

	var exitCode int
	defer func() {
		os.Exit(exitCode)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		infof("%s", colorText(colorInfo, "Cancelling pending tasks..."))
		os.Stdin.Close()
		cancel()
	}()

	switch cfg.command {
	case "refresh", "ref":
		if err := handleRefresh(ctx, tmpDir); err != nil {
			exitCode = 1
		}
	case "dist-upgrade", "dup":
		if err := handleDistUpgrade(ctx, tmpDir); err != nil {
			exitCode = 1
		}
	case "update", "up":
		if err := handleUpdate(ctx, tmpDir); err != nil {
			exitCode = 1
		}
	case "search", "se":
		if err := handleSearch(ctx, tmpDir); err != nil {
			var see *searchExitError
			if errors.As(err, &see) {
				exitCode = see.code
			} else {
				exitCode = 1
			}
		}
	case "install", "in":
		if err := handleInstall(ctx, tmpDir); err != nil {
			exitCode = 1
		}
	case "install-new-recommends", "inr":
		if err := handleINR(ctx, tmpDir); err != nil {
			exitCode = 1
		}
	default:
		printHelp()
		exitCode = 1
	}
}

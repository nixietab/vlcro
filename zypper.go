package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// zypper host execution (no shell, no bash)

func zypperHostExec(args []string) (string, int) {
	cmd := exec.Command("zypper", args...)
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=/root",
		"LANG=C.UTF-8",
	}
	output, err := cmd.Output()
	out := strings.TrimSpace(string(output))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return out, exitErr.ExitCode()
		}
		return out, -1
	}
	return out, 0
}

func zypperDryRun(extraArgs []string) (string, int) {
	args := []string{"--non-interactive", "--no-cd", "--xmlout"}
	args = append(args, extraArgs...)
	cmd := exec.Command("zypper", args...)
	output, err := cmd.Output()
	out := strings.TrimSpace(string(output))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return out, exitErr.ExitCode()
		}
		return out, -1
	}
	return out, 0
}

// zypper chroot execution

func runZypper(ctx context.Context, rootfs string, args ...string) (stdout, stderr string, exitCode int, err error) {
	info, err := os.Stat(rootfs)
	if err != nil {
		return "", "", -1, fmt.Errorf("rootfs does not exist: %w", err)
	}
	if !info.IsDir() {
		return "", "", -1, fmt.Errorf("rootfs is a not a directory")
	}

	cmd := exec.CommandContext(ctx, "/usr/bin/zypper", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: rootfs}
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"ZYPP_CURL2=1",
		"ZYPP_PCK_PRELOAD=1",
		"ZYPP_SINGLE_RPMTRANS=1",
		"HOME=/root",
		"LANG=C.UTF-8",
	}
	cmd.Stdin = nil

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}

// handover (terminal passthrough)

func handover(command string) int {
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

// xml parsing helpers

func findElements(xmlData, name string) []map[string]string {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	var results []map[string]string
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == name {
			attrs := make(map[string]string)
			for _, attr := range se.Attr {
				attrs[attr.Name.Local] = attr.Value
			}
			results = append(results, attrs)
		}
	}
	return results
}

func findMessages(xmlData string) []string {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	var results []string
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "message" {
			var text strings.Builder
			for {
				innerTok, err := decoder.Token()
				if err == io.EOF || err != nil {
					break
				}
				if ee, ok := innerTok.(xml.EndElement); ok && ee.Name.Local == "message" {
					break
				}
				if cd, ok := innerTok.(xml.CharData); ok {
					text.Write(cd)
				}
			}
			results = append(results, text.String())
		}
	}
	return results
}

func parseInstallSummary(xmlData string) (downloadSize, diffBytes float64, numPkgs int, found bool) {
	for _, s := range findElements(xmlData, "install-summary") {
		debugf("install-summary attrs: download-size=%q space-usage-diff=%q packages-to-change=%q",
			s["download-size"], s["space-usage-diff"], s["packages-to-change"])
		fmt.Sscanf(s["download-size"], "%f", &downloadSize)
		fmt.Sscanf(s["space-usage-diff"], "%f", &diffBytes)
		fmt.Sscanf(s["packages-to-change"], "%d", &numPkgs)
		found = true
	}
	if !found {
		return
	}
	if downloadSize == 0 {
		downloadSize = findChildTextFloat(xmlData, "install-summary", "download-size")
	}
	if diffBytes == 0 {
		diffBytes = findChildTextFloat(xmlData, "install-summary", "space-usage-diff")
	}
	if numPkgs == 0 {
		numPkgs = int(findChildTextFloat(xmlData, "install-summary", "packages-to-change"))
	}
	return
}

func findChildTextFloat(xmlData, parent, child string) float64 {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	inParent := 0
	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == parent {
				inParent++
			} else if inParent > 0 && t.Name.Local == child {
				var text strings.Builder
				for {
					innerTok, err := decoder.Token()
					if err != nil {
						break
					}
					if ee, ok := innerTok.(xml.EndElement); ok && ee.Name.Local == child {
						break
					}
					if cd, ok := innerTok.(xml.CharData); ok {
						text.Write(cd)
					}
				}
				var val float64
				fmt.Sscanf(strings.TrimSpace(text.String()), "%f", &val)
				return val
			}
		case xml.EndElement:
			if t.Name.Local == parent && inParent > 0 {
				inParent--
			}
		}
	}
	return 0
}

func parseAllSolvables(xmlData string) []packageInfo {
	var pkgs []packageInfo
	for _, s := range findElements(xmlData, "solvable") {
		if s["type"] == "package" {
			var size float64
			fmt.Sscanf(s["size"], "%f", &size)
			pkgs = append(pkgs, packageInfo{
				Name:    s["name"],
				Edition: s["edition"],
				Arch:    s["arch"],
				Action:  "install",
				Size:    size,
			})
		}
	}
	return pkgs
}

func parseToUpdate(xmlData string) []packageInfo {
	var pkgs []packageInfo
	for _, u := range findElements(xmlData, "update") {
		if u["kind"] == "package" {
			var size float64
			fmt.Sscanf(u["size"], "%f", &size)
			pkgs = append(pkgs, packageInfo{
				Name:    u["name"],
				Edition: u["edition"],
				Arch:    u["arch"],
				Action:  "upgrade",
				Size:    size,
			})
		}
	}
	return pkgs
}

func parseToRemove(xmlData string) []packageInfo {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	var results []packageInfo
	inToRemove := 0
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "to-remove" {
				inToRemove++
			} else if inToRemove > 0 && t.Name.Local == "solvable" {
				attrs := make(map[string]string)
				for _, attr := range t.Attr {
					attrs[attr.Name.Local] = attr.Value
				}
				if attrs["type"] == "package" {
					results = append(results, packageInfo{
						Name:    attrs["name"],
						Edition: attrs["edition"],
						Arch:    attrs["arch"],
						Action:  "remove",
					})
				}
			}
		case xml.EndElement:
			if t.Name.Local == "to-remove" {
				inToRemove--
			}
		}
	}
	return results
}

func filterPackages(allPkgs, rmPkgs []packageInfo) []packageInfo {
	rmSet := make(map[string]bool, len(rmPkgs))
	for _, p := range rmPkgs {
		rmSet[p.String()] = true
	}
	var result []packageInfo
	for _, p := range allPkgs {
		if !rmSet[p.String()] {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].String() < result[j].String()
	})
	return result
}

// shared handler logic

type transactionInfo struct {
	installs     []packageInfo
	removes      []packageInfo
	downloadSize float64
	diffBytes    float64
	numPkgs      int
	nothingToDo  bool
}

func dryRunAndParse(extraArgs []string) (*transactionInfo, error) {
	xmlOutput, ret := zypperDryRun(extraArgs)
	debugf("%s", xmlOutput)

	if strings.Contains(xmlOutput, "Nothing to do") {
		return &transactionInfo{nothingToDo: true}, nil
	}

	downloadSize, diffBytes, numPkgs, found := parseInstallSummary(xmlOutput)

	if ret != 0 {
		if !found || numPkgs == 0 {
			msgs := findMessages(xmlOutput)
			if len(msgs) > 0 {
				return nil, fmt.Errorf("zypper error:\n%s", strings.Join(msgs, "\n"))
			}
			return nil, fmt.Errorf("zypper dry-run failed with exit code %d", ret)
		}
	}

	if !found || numPkgs == 0 {
		return &transactionInfo{nothingToDo: true}, nil
	}

	installs := parseAllSolvables(xmlOutput)
	removes := parseToRemove(xmlOutput)
	filtered := filterPackages(installs, removes)

	return &transactionInfo{
		installs:     filtered,
		removes:      removes,
		downloadSize: downloadSize,
		diffBytes:    diffBytes,
		numPkgs:      numPkgs,
	}, nil
}

func showTransactionAndConfirm(info *transactionInfo) bool {
	if info.nothingToDo {
		infof("%s", colorText(colorInfo, "Nothing to do. Exiting..."))
		return false
	}

	printTransactionSummary(info.installs, info.removes, info.downloadSize, info.diffBytes, info.numPkgs)

	fmt.Println()
	if cfg.noConfirm {
		return true
	}
	return queryYesNo("Do you want to continue?", "yes")
}

func pkgNames(pkgs []packageInfo) []string {
	names := make([]string, len(pkgs))
	for i, p := range pkgs {
		names[i] = p.String()
	}
	return names
}

// worker pool

type job struct {
	item        string
	itemCounter int
	totalItems  int
	workerIdx   int
	size        float64
}

func makeLogMessages(item string, counter, total int) map[string]string {
	m := make(map[string]string)
	forceStr := ""
	if cfg.force {
		forceStr = "force "
	}
	switch cfg.command {
	case "refresh", "ref":
		m["error"] = fmt.Sprintf("Error %srefreshing repo [%d/%d] %q", forceStr, counter, total, item)
		m["exception"] = fmt.Sprintf("SIGINT while %srefreshing repo [%d/%d] %q", forceStr, counter, total, item)
	default:
		m["error"] = fmt.Sprintf("Error downloading package [%d/%d] %q", counter, total, item)
		m["exception"] = fmt.Sprintf("SIGINT while downloading package [%d/%d] %q", counter, total, item)
	}
	return m
}

func refreshArgs() []string {
	if cfg.force {
		return []string{"refresh", "--force"}
	}
	return []string{"refresh"}
}

func worker(ctx context.Context, wg *sync.WaitGroup, uuidPool chan string, tmpDir string, jobsChan chan job) {
	defer wg.Done()
	for j := range jobsChan {
		select {
		case <-ctx.Done():
			return
		default:
		}

		uuid := <-uuidPool
		msgs := makeLogMessages(j.item, j.itemCounter, j.totalItems)

		if prog != nil {
			prog.increment()
		}
		downloadLine(j.itemCounter, j.item, j.size)

		rootfs := filepath.Join(tmpDir, uuid, "rootfs")
		var zypperArgs []string
		if cfg.command == "refresh" || cfg.command == "ref" {
			zypperArgs = []string{"--non-interactive"}
			zypperArgs = append(zypperArgs, refreshArgs()...)
			zypperArgs = append(zypperArgs, j.item)
		} else {
			zypperArgs = []string{"--non-interactive", "download", j.item}
		}

		var stdout, stderr string
		var exitCode int
		var err error

		for attempt := 1; attempt <= maxRetries; attempt++ {
			stdout, stderr, exitCode, err = runZypper(ctx, rootfs, zypperArgs...)

			if ctx.Err() != nil {
				debugf("%s", msgs["exception"])
				uuidPool <- uuid
				return
			}

			if err == nil && exitCode == 0 {
				break
			}

			if attempt < maxRetries {
				warningf("Attempt %d failed for %q. Retrying...", attempt, j.item)
				time.Sleep(1 * time.Second)
			} else {
				errorf("%s", colorText(colorError, fmt.Sprintf("%s. zypper exit code: %d", msgs["error"], exitCode)))
				if stderr != "" {
					debugf("[stderr]\n%s", stderr)
				}
				if stdout != "" {
					debugf("[stdout]\n%s", stdout)
				}
			}
		}

		uuidPool <- uuid
	}
}

func mainTask(ctx context.Context, taskItems []string, tmpDir string, needDev bool, tracker *MountTracker) error {
	numWorkers := cfg.jobs
	if len(taskItems) < numWorkers {
		numWorkers = len(taskItems)
	}

	uuidPool := make(chan string, numWorkers)
	allUUIDs := make([]string, numWorkers)

	for i := 0; i < numWorkers; i++ {
		u := randomUUID()
		allUUIDs[i] = u
		uuidPool <- u
	}

	if needDev {
		for _, uuid := range allUUIDs {
			if err := setupChroot(tmpDir, uuid, needDev, tracker); err != nil {
				errorf("%s", colorText(colorError, fmt.Sprintf("Chroot setup failed: %v", err)))
				return err
			}
		}
	} else {
		var setupWg sync.WaitGroup
		setupErrs := make(chan error, numWorkers)
		for _, uuid := range allUUIDs {
			setupWg.Add(1)
			go func(u string) {
				defer setupWg.Done()
				if err := setupChroot(tmpDir, u, needDev, tracker); err != nil {
					setupErrs <- err
				}
			}(uuid)
		}
		setupWg.Wait()
		close(setupErrs)
		if err := <-setupErrs; err != nil {
			errorf("%s", colorText(colorError, fmt.Sprintf("Chroot setup failed: %v", err)))
			return err
		}
	}

	totalItems := len(taskItems)

	totalBytes := float64(0)
	if cfg.txInfo != nil {
		for _, p := range cfg.txInfo {
			totalBytes += p.Size
		}
	}

	pkgSizes := make(map[string]float64)
	if cfg.txInfo != nil {
		for _, p := range cfg.txInfo {
			pkgSizes[p.String()] = p.Size
		}
	}

	prog = newProgressState(totalItems)
	prog.startTicker()
	defer func() {
		prog.finish()
		prog = nil
	}()

	jobsChan := make(chan job, totalItems)

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(ctx, &wg, uuidPool, tmpDir, jobsChan)
	}

	itemCounter := 0
	for _, item := range taskItems {
		itemCounter++
		size := pkgSizes[item]
		select {
		case <-ctx.Done():
			close(jobsChan)
			wg.Wait()
			return ctx.Err()
		case jobsChan <- job{item: item, itemCounter: itemCounter, totalItems: totalItems, workerIdx: (itemCounter - 1) % numWorkers, size: size}:
		}
	}

	close(jobsChan)
	wg.Wait()

	if totalBytes > 0 {
		fetchSummary(totalBytes, time.Since(prog.startTime))
	}

	return nil
}

// prompt

func queryYesNo(question, defaultVal string) bool {
	valid := map[string]bool{"yes": true, "y": true, "ye": true, "no": false, "n": false}
	var prompt string
	switch defaultVal {
	case "":
		prompt = " [y/n]: "
	case "yes":
		prompt = " [Y/n]: "
	case "no":
		prompt = " [y/N]: "
	default:
		prompt = " [y/n]: "
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(colorText(colorInput, question+prompt))
		choice, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			return false
		}
		choice = strings.ToLower(strings.TrimSpace(choice))
		if defaultVal != "" && choice == "" {
			return valid[defaultVal]
		}
		if v, ok := valid[choice]; ok {
			return v
		}
		fmt.Print(colorText(colorWarning, "Please respond with 'yes' or 'no' (or 'y' or 'n').\n"))
	}
}

// zypp lock

func getZyppLock() int {
	ourPid := os.Getpid()
	os.MkdirAll(filepath.Dir(zypperPidFile), 0755)
	os.WriteFile(zypperPidFile, []byte(fmt.Sprintf("%d", ourPid)), 0644)
	return ourPid
}

func releaseZyppLock() bool {
	if os.Getuid() == 0 {
		if err := os.WriteFile(zypperPidFile, []byte{}, 0644); err != nil {
			debugf("Warning: failed to clear zypp lock file: %v", err)
		}
		return true
	}
	return false
}

func checkZypperRunning() (int, string) {
	data, err := os.ReadFile(zypperPidFile)
	if err != nil {
		return 0, ""
	}
	pidStr := strings.TrimSpace(string(data))
	if pidStr == "" {
		return 0, ""
	}
	var pid int
	fmt.Sscanf(pidStr, "%d", &pid)
	if pid == 0 {
		return 0, ""
	}

	commPath := fmt.Sprintf("/proc/%d/comm", pid)
	commData, err := os.ReadFile(commPath)
	if err != nil {
		return 0, ""
	}
	program := strings.TrimSpace(string(commData))
	if program == "" {
		return 0, ""
	}
	return pid, program
}

// command handlers

func handleRefresh(ctx context.Context, tmpDir string) error {
	tracker := &MountTracker{}
	defer vlcroCleanup(tmpDir, tracker)

	statusMsg("Reading package lists")
	xmlOutput, _ := zypperDryRun([]string{"repos"})
	statusMsg("Enumerating enabled repos")

	getZyppLock()

	repos := findElements(xmlOutput, "repo")
	var aliases []string
	for _, r := range repos {
		if r["enabled"] == "1" {
			aliases = append(aliases, r["alias"])
		}
	}
	debugf("Enabled repos: %v", aliases)

	if len(aliases) == 0 {
		infof("%s", colorText(colorInfo, "No repos found. Exiting..."))
		return nil
	}

	return mainTask(ctx, aliases, tmpDir, true, tracker)
}

func handleDistUpgrade(ctx context.Context, tmpDir string) error {
	tracker := &MountTracker{}
	defer vlcroCleanup(tmpDir, tracker)

	statusMsg("Reading package lists")
	statusMsg("Building dependency tree")
	info, err := dryRunAndParse([]string{"dist-upgrade", "--dry-run"})
	if err != nil {
		errorf("%s", colorText(colorError, fmt.Sprintf("%v\nThere are package conflicts that must be manually resolved. See output of:\nzypper --non-interactive --no-cd dist-upgrade --dry-run", err)))
		return err
	}

	if info.nothingToDo {
		infof("%s", colorText(colorInfo, "Nothing to do. Exiting..."))
		return nil
	}

	cfg.txInfo = info.installs

	if !showTransactionAndConfirm(info) {
		return nil
	}

	getZyppLock()

	if info.downloadSize > 0 {
		statusMsg("Downloading packages")
		err := mainTask(ctx, pkgNames(info.installs), tmpDir, false, tracker)
		if err == context.Canceled {
			return err
		}
		if err != nil {
			warningf("%s", colorText(colorWarning, fmt.Sprintf("Parallel execution failed (%v). Falling back to standard zypper...", err)))
		}
	}

	vlcroCleanup(tmpDir, tracker)

	if !cfg.downloadOnly {
		infof("%s", colorText(colorInfo, "Vlcro has finished its tasks. Handing you over to zypper..."))
		nonInteractive := ""
		if cfg.noConfirm {
			nonInteractive = "--non-interactive"
		}
		code := handover(fmt.Sprintf("env ZYPP_CURL2=1 ZYPP_PCK_PRELOAD=1 ZYPP_SINGLE_RPMTRANS=1 zypper %s --no-cd dist-upgrade", nonInteractive))
		if code != 0 {
			return fmt.Errorf("zypper dist-upgrade failed with exit code %d", code)
		}
	}

	return nil
}

func handleUpdate(ctx context.Context, tmpDir string) error {
	tracker := &MountTracker{}
	defer vlcroCleanup(tmpDir, tracker)

	statusMsg("Reading package lists")
	statusMsg("Building dependency tree")
	info, err := dryRunAndParse([]string{"update", "--dry-run"})
	if err != nil {
		errorf("%s", colorText(colorError, fmt.Sprintf("Error: %v", err)))
		return err
	}

	if info.nothingToDo {
		infof("%s", colorText(colorInfo, "Nothing to do. Exiting..."))
		return nil
	}

	cfg.txInfo = info.installs

	if !showTransactionAndConfirm(info) {
		return nil
	}

	getZyppLock()

	if info.downloadSize > 0 {
		statusMsg("Downloading packages")
		err := mainTask(ctx, pkgNames(info.installs), tmpDir, false, tracker)
		if err == context.Canceled {
			return err
		}
		if err != nil {
			warningf("%s", colorText(colorWarning, fmt.Sprintf("Parallel execution failed (%v). Falling back to standard zypper...", err)))
		}
	}

	vlcroCleanup(tmpDir, tracker)

	if !cfg.downloadOnly {
		infof("%s", colorText(colorInfo, "Vlcro has finished its tasks. Handing you over to zypper..."))
		nonInteractive := ""
		if cfg.noConfirm {
			nonInteractive = "--non-interactive"
		}
		code := handover(fmt.Sprintf("env ZYPP_CURL2=1 ZYPP_PCK_PRELOAD=1 ZYPP_SINGLE_RPMTRANS=1 zypper %s --no-cd update", nonInteractive))
		if code != 0 {
			return fmt.Errorf("zypper update failed with exit code %d", code)
		}
	}

	return nil
}

func handleInstall(ctx context.Context, tmpDir string) error {
	if len(cfg.packages) == 0 {
		errorf("%s", colorText(colorError, "No packages specified for installation"))
		return fmt.Errorf("no packages specified")
	}

	tracker := &MountTracker{}
	defer vlcroCleanup(tmpDir, tracker)

	statusMsg("Reading package lists")
	statusMsg("Building dependency tree")
	dryRunArgs := []string{"install", "--dry-run"}
	dryRunArgs = append(dryRunArgs, cfg.packages...)
	info, err := dryRunAndParse(dryRunArgs)
	if err != nil {
		errorf("%s", colorText(colorError, fmt.Sprintf("Error: %v", err)))
		return err
	}

	if info.nothingToDo {
		infof("%s", colorText(colorInfo, "Nothing to do. Exiting..."))
		return nil
	}

	cfg.txInfo = info.installs

	if !showTransactionAndConfirm(info) {
		return nil
	}

	getZyppLock()

	if info.downloadSize > 0 {
		statusMsg("Downloading packages")
		err := mainTask(ctx, pkgNames(info.installs), tmpDir, false, tracker)
		if err == context.Canceled {
			return err
		}
		if err != nil {
			warningf("%s", colorText(colorWarning, fmt.Sprintf("Parallel execution failed (%v). Falling back to standard zypper...", err)))
		}
	}

	vlcroCleanup(tmpDir, tracker)

	if !cfg.downloadOnly {
		infof("%s", colorText(colorInfo, "Vlcro has finished its tasks. Handing you over to zypper..."))
		nonInteractive := ""
		if cfg.noConfirm {
			nonInteractive = "--non-interactive"
		}
		code := handover(fmt.Sprintf("env ZYPP_CURL2=1 ZYPP_PCK_PRELOAD=1 ZYPP_SINGLE_RPMTRANS=1 zypper %s --no-cd install %s", nonInteractive, strings.Join(cfg.packages, " ")))
		if code != 0 {
			return fmt.Errorf("zypper install failed with exit code %d", code)
		}
	}

	return nil
}

func handleINR(ctx context.Context, tmpDir string) error {
	tracker := &MountTracker{}
	defer vlcroCleanup(tmpDir, tracker)

	statusMsg("Reading package lists")
	statusMsg("Building dependency tree")
	info, err := dryRunAndParse([]string{"install-new-recommends", "--dry-run"})
	if err != nil {
		errorf("%s", colorText(colorError, fmt.Sprintf("Error: %v", err)))
		return err
	}

	if info.nothingToDo {
		infof("%s", colorText(colorInfo, "Nothing to do. Exiting..."))
		return nil
	}

	cfg.txInfo = info.installs

	if !showTransactionAndConfirm(info) {
		return nil
	}

	getZyppLock()

	if info.downloadSize > 0 {
		statusMsg("Downloading packages")
		err := mainTask(ctx, pkgNames(info.installs), tmpDir, false, tracker)
		if err == context.Canceled {
			return err
		}
		if err != nil {
			warningf("%s", colorText(colorWarning, fmt.Sprintf("Parallel execution failed (%v). Falling back to standard zypper...", err)))
		}
	}

	vlcroCleanup(tmpDir, tracker)

	if !cfg.downloadOnly {
		infof("%s", colorText(colorInfo, "Vlcro has finished its tasks. Handing you over to zypper..."))
		nonInteractive := ""
		if cfg.noConfirm {
			nonInteractive = "--non-interactive"
		}
		code := handover(fmt.Sprintf("env ZYPP_CURL2=1 ZYPP_PCK_PRELOAD=1 ZYPP_SINGLE_RPMTRANS=1 zypper %s --no-cd install-new-recommends", nonInteractive))
		if code != 0 {
			return fmt.Errorf("zypper install-new-recommends failed with exit code %d", code)
		}
	}

	return nil
}

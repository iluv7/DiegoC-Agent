package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"diegoc-agent/internal/logger"
)

func runLogSubcommand(filename string) {
	l := logger.New()
	logDir := l.LogDir()
	fmt.Printf("\n%s📁 Log directory: %s%s\n", cBrightCyan, logDir, cReset)
	info, err := os.Stat(logDir)
	if err != nil || !info.IsDir() {
		fmt.Printf("%sLog directory does not exist or is not a directory.%s\n", cRed, cReset)
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		fmt.Printf("%sError reading log dir: %v%s\n", cRed, err, cReset)
		return
	}
	var logFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e)
		}
	}
	sort.Slice(logFiles, func(i, j int) bool {
		ii, _ := logFiles[i].Info()
		jj, _ := logFiles[j].Info()
		return ii.ModTime().After(jj.ModTime())
	})
	if filename == "" {
		fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 60)+cReset)
		fmt.Printf("%sAvailable log files (newest first):%s\n", cBold+cBrightYellow, cReset)
		max := 10
		if len(logFiles) < max {
			max = len(logFiles)
		}
		for i := 0; i < max; i++ {
			inf, _ := logFiles[i].Info()
			fmt.Printf("  %s%2d.%s %s  %s(modified: %s)%s\n", cGreen, i+1, cReset, cBrightWhite+logFiles[i].Name()+cReset, cDim, inf.ModTime().Format("2006-01-02 15:04:05"), cReset)
		}
		if len(logFiles) > 10 {
			fmt.Printf("  %s... and %d more%s\n", cDim, len(logFiles)-10, cReset)
		}
		fmt.Printf("%s%s%s\n", cDim, strings.Repeat("─", 60), cReset)
		return
	}
	path := filepath.Join(logDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%sLog file not found or unreadable: %s%s\n", cRed, path, cReset)
		return
	}
	fmt.Printf("\n%s📄 Reading: %s%s\n", cBrightCyan, path, cReset)
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 80)+cReset)
	fmt.Println(string(data))
	fmt.Printf("%s%s%s\n", cDim, strings.Repeat("─", 80), cReset)
}

func showLogDirectory(l *logger.AgentLogger) {
	logDir := l.LogDir()
	fmt.Printf("\n%s📁 Log directory: %s%s\n\n", cBrightCyan, logDir, cReset)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		fmt.Printf("%sLog directory does not exist or is not readable.%s\n\n", cRed, cReset)
		return
	}
	var logFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e)
		}
	}
	sort.Slice(logFiles, func(i, j int) bool {
		ii, _ := logFiles[i].Info()
		jj, _ := logFiles[j].Info()
		return ii.ModTime().After(jj.ModTime())
	})
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 60)+cReset)
	fmt.Printf("%sAvailable log files (newest first):%s\n", cBold+cBrightYellow, cReset)
	for i := 0; i < len(logFiles) && i < 10; i++ {
		inf, _ := logFiles[i].Info()
		fmt.Printf("  %s%2d.%s %s  %s(%s)%s\n", cGreen, i+1, cReset, cBrightWhite+logFiles[i].Name()+cReset, cDim, inf.ModTime().Format("2006-01-02 15:04:05"), cReset)
	}
	if len(logFiles) > 10 {
		fmt.Printf("  %s... and %d more%s\n", cDim, len(logFiles)-10, cReset)
	}
	fmt.Printf("%s%s%s\n\n", cDim, strings.Repeat("─", 60), cReset)
}

func readLogFile(logDir, filename string) {
	path := filepath.Join(logDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%s❌ Log file not found: %s%s\n\n", cRed, path, cReset)
		return
	}
	fmt.Printf("\n%s📄 Reading: %s%s\n", cBrightCyan, path, cReset)
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 80)+cReset)
	fmt.Println(string(data))
	fmt.Printf("%s%s%s\n\n", cDim, strings.Repeat("─", 80), cReset)
}

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"pendriverescue/internal/webui"
)

// runGUI serves the app on 127.0.0.1 and opens it in the web browser. The
// console window stays open: closing it quits the program.
func runGUI(f flags) {
	if addr := webui.Running(); addr != "" {
		fmt.Println("Pendrive Rescue is already running. Its window is opening in your browser.")
		fmt.Println("  " + addr)
		openURL(addr)
		time.Sleep(3 * time.Second)
		return
	}
	srv := webui.New(webui.Config{Version: version, OpenPath: openFolder})
	l, addr, err := srv.Listen(f.port)
	if err != nil {
		fmt.Println("The browser window could not be started:", err)
		fmt.Println("Using the console instead.")
		fmt.Println()
		runCLI(f)
		return
	}
	webui.SaveLock(addr)
	defer webui.RemoveLock(addr)

	fmt.Println("==============================================================")
	fmt.Printf("   PENDRIVE RESCUE %s is open in your web browser.\n", version)
	fmt.Println("   It only READS your pendrive. Nothing on it is changed.")
	fmt.Println("==============================================================")
	fmt.Println()
	fmt.Println("If the browser did not open, copy this address into it:")
	fmt.Println()
	fmt.Println("   " + addr)
	fmt.Println()
	fmt.Println("Keep this window open while you use Pendrive Rescue.")
	fmt.Println("To quit, press Quit in the browser or close this window.")
	if runtime.GOOS == "windows" {
		fmt.Println()
		fmt.Println("If Windows asks you to FORMAT the pendrive, always click CANCEL.")
	}

	go func() {
		if err := srv.Serve(l); err != nil {
			fmt.Println("Error:", err)
		}
	}()
	if !f.noBrowser {
		openURL(addr)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-srv.Done():
		fmt.Println()
		fmt.Println("Pendrive Rescue was closed from the browser.")
	case <-stop:
	}
	srv.Close()
}

// openURL opens the address in the user's normal browser. On Windows it goes
// through Explorer, so the browser does not run with administrator rights.
func openURL(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer.exe", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		asUser("xdg-open", u)
	}
}

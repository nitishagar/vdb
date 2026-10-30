// Command fakevda is a test double for the VDA binary. It speaks the
// adb-derived surface vdb's vda adapter expects:
//
//	fakevda devices -l
//	fakevda version
//	fakevda -s SERIAL forward tcp:0 tcp:8383      # prints chosen local port
//	fakevda -s SERIAL forward --remove tcp:PORT
//	fakevda -s SERIAL shell CMD
//	fakevda -s SERIAL push LOCAL REMOTE
//	fakevda -s SERIAL pull REMOTE LOCAL
//
// Environment:
//
//	FAKEVDA_STATE_DIR     directory holding devices.txt (one "<serial> <kind> …" per line)
//	FAKEVDA_TOOLKIT_ADDR  host:port the forward proxies to (the fake toolkit)
//
// Forwards are held by a detached child process (like the adb server model);
// `forward --remove` kills that child, releasing the listener.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "fakevda: missing command")
		os.Exit(2)
	}
	// Internal mode: the detached listener owner (args: <port> <backend>).
	if os.Getenv("FAKEVDA_SERVE_FORWARD") == "1" && len(os.Args) == 3 {
		serveForward(os.Args[1], os.Args[2])
		return
	}

	var serial string
	args := os.Args[1:]
	if args[0] == "-s" {
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "fakevda: -s requires a serial")
			os.Exit(2)
		}
		serial = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fakevda: missing command")
		os.Exit(2)
	}

	switch args[0] {
	case "devices":
		devices()
	case "version":
		fmt.Println("Vega Device Adapter version 9.9-fake Server Version 41 Platform Version 34.0.4")
	case "forward":
		forward(serial, args[1:])
	case "shell":
		fmt.Printf("fake-shell(%s): %s\n", serial, strings.Join(args[1:], " "))
	case "push":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "fakevda: push LOCAL REMOTE")
			os.Exit(2)
		}
		if err := copyFile(args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s pushed to %s\n", args[1], args[2])
	case "pull":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "fakevda: pull REMOTE LOCAL")
			os.Exit(2)
		}
		if err := copyFile(args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s pulled to %s\n", args[1], args[2])
	default:
		fmt.Fprintf(os.Stderr, "fakevda: unknown command %q\n", args[0])
		os.Exit(2)
	}
}

func stateDir() string {
	d := os.Getenv("FAKEVDA_STATE_DIR")
	if d == "" {
		fmt.Fprintln(os.Stderr, "fakevda: FAKEVDA_STATE_DIR not set")
		os.Exit(2)
	}
	return d
}

func devices() {
	fmt.Println("List of devices attached")
	data, err := os.ReadFile(filepath.Join(stateDir(), "devices.txt"))
	if err != nil {
		return // no devices file = empty list, still valid output
	}
	fmt.Print(string(data))
	fmt.Println()
}

func forward(serial string, args []string) {
	if serial == "" {
		fmt.Fprintln(os.Stderr, "fakevda: forward requires -s SERIAL")
		os.Exit(2)
	}
	if len(args) == 2 && args[0] == "--remove" {
		removeForward(args[1])
		return
	}
	// forward tcp:<local|0> tcp:<devicePort> — args here exclude "forward".
	if len(args) != 2 || !strings.HasPrefix(args[0], "tcp:") || !strings.HasPrefix(args[1], "tcp:") {
		fmt.Fprintln(os.Stderr, "fakevda: usage: forward tcp:0 tcp:PORT")
		os.Exit(2)
	}
	localSpec := strings.TrimPrefix(args[0], "tcp:")
	backendPort := strings.TrimPrefix(args[1], "tcp:")

	backend := os.Getenv("FAKEVDA_TOOLKIT_ADDR")
	if backend == "" {
		backend = "127.0.0.1:" + backendPort
	}

	// Bind in the parent so the chosen port is known and reserved, then pass
	// the listener fd to a detached child that owns it (no re-bind race).
	ln, err := net.Listen("tcp", "127.0.0.1:"+localSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakevda: listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	listenerFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakevda: listener fd: %v\n", err)
		os.Exit(1)
	}

	dir := stateDir()
	pidFile := filepath.Join(dir, fmt.Sprintf("forward-%d.pid", port))
	cmd := os.Args[0]
	attr := &os.ProcAttr{
		Dir: dir,
		Env: append(os.Environ(), "FAKEVDA_SERVE_FORWARD=1"),
		// The detached child must NOT inherit our stdout/stderr pipes: the
		// parent's caller (execrun) waits for pipe EOF, and a long-lived
		// forward child holding the write end would deadlock Forward().
		// nil entries become /dev/null; fd 3 carries the listener.
		Files: []*os.File{nil, nil, nil, listenerFile},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	}
	proc, err := os.StartProcess(cmd, []string{cmd, strconv.Itoa(port), backend}, attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakevda: start forward child: %v\n", err)
		os.Exit(1)
	}
	// Parent closes its copy; the child holds the listener via fd 3.
	_ = listenerFile.Close()
	_ = ln.Close()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Pid)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fakevda: write pid: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(port)
}

func removeForward(spec string) {
	if !strings.HasPrefix(spec, "tcp:") {
		fmt.Fprintln(os.Stderr, "fakevda: --remove expects tcp:PORT")
		os.Exit(2)
	}
	port := strings.TrimPrefix(spec, "tcp:")
	pidFile := filepath.Join(stateDir(), fmt.Sprintf("forward-%s.pid", port))
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return // already gone: idempotent
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	_ = os.Remove(pidFile)
}

// serveForward is the detached child: owns the inherited listener (fd 3),
// proxies TCP to the fake toolkit, exits when the listener dies.
func serveForward(portStr, backend string) {
	port, err := strconv.Atoi(portStr)
	if err != nil {
		os.Exit(2)
	}
	f := os.NewFile(3, fmt.Sprintf("listen-%d", port))
	ln, err := net.FileListener(f)
	if err != nil {
		os.Exit(1)
	}
	_ = f.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			up, err := net.Dial("tcp", backend)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
		}(conn)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

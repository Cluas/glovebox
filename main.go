// Glovebox is a LuCI-style web console for rooted Android pocket Wi-Fi / mobile routers
// (currently the ZTE F50). It runs as root on the stock firmware and talks to the vendor goform
// API, the mihomo API and /proc, /sys.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Android has no /usr/share/zoneinfo
)

var version = "dev"

const usage = `Usage: glovebox [-config file] [-log file] <command>

Commands:
  serve                 run the console (default)
  passwd                read a new console password from the first line of stdin
  config                print the effective configuration (without secrets)
  config import         merge JSON from stdin into the configuration file
  demo [-listen addr]   run without a device (fake backends, synthetic data), default 127.0.0.1:8080
  version               print the version

Options:
`

func main() {
	cfgPath := flag.String("config", "/data/adb/glovebox/config.json", "configuration file")
	logPath := flag.String("log", "", "log file (default: glovebox.log next to the config file, - for stderr)")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	switch cmd := flag.Arg(0); cmd {
	case "version":
		fmt.Println(version)
		return
	case "passwd":
		exitOn(setPassword(*cfgPath))
		return
	case "config":
		exitOn(configCmd(*cfgPath, flag.Args()[1:]))
		return
	case "demo":
		exitOn(runDemo(flag.Args()[1:]))
		return
	case "", "serve":
	default:
		flag.Usage()
		os.Exit(2)
	}

	logDir := filepath.Dir(*cfgPath)
	if *logPath != "-" {
		p := *logPath
		if p == "" {
			p = filepath.Join(logDir, logFileName)
		}
		logDir = filepath.Dir(p)
		lw, err := newRotatingLog(p, 1<<20)
		if err != nil {
			log.Fatal(err)
		}
		log.SetOutput(lw)
	}
	setLocalTZ()
	// PATH may be incomplete when started by Magisk; ip, getprop and logcat live in /system/bin
	if _, err := os.Stat("/system/bin"); err == nil && !strings.Contains(os.Getenv("PATH"), "/system/bin") {
		os.Setenv("PATH", os.Getenv("PATH")+":/system/bin:/system/xbin")
	}

	cs, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("read config %s: %v", *cfgPath, err)
	}
	c := cs.get()
	if c.PasswordHash == "" {
		log.Printf("no console password set yet: glovebox -config %s passwd", *cfgPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := newServer(cs, logDir)
	if len(c.TLSListen) > 0 {
		if cm, err := newCertManager(filepath.Join(filepath.Dir(*cfgPath), "tls"), c); err != nil {
			log.Printf("HTTPS unavailable: %v", err)
		} else {
			s.certs = cm
		}
	}
	if err := s.run(ctx); err != nil {
		log.Fatal(err)
	}
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func configCmd(cfgPath string, args []string) error {
	cs, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0:
		b, _ := json.MarshalIndent(cs.get().redacted(), "", "  ")
		fmt.Println(string(b))
		return nil
	case len(args) == 1 && args[0] == "import":
		if err := cs.importJSON(os.Stdin); err != nil {
			return err
		}
		fmt.Println("configuration updated; restart glovebox to apply")
		return nil
	}
	return fmt.Errorf("usage: glovebox config [import]")
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	fs.Parse(args)
	log.SetOutput(os.Stderr)
	dir, err := os.MkdirTemp("", "glovebox-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s, err := newDemoServer(ctx, dir, *listen)
	if err != nil {
		return err
	}
	log.Printf("demo mode: open http://%s and log in with %q", *listen, demoPassword)
	return s.run(ctx)
}

func setPassword(cfgPath string) error {
	cs, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read password from stdin: %w", err)
	}
	pw := strings.TrimRight(line, "\r\n")
	if len(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	if err := cs.update(func(c *Config) { c.PasswordHash = h }); err != nil {
		return err
	}
	fmt.Println("console password set")
	return nil
}

// Android keeps the time zone in persist.sys.timezone, not /etc/localtime
func setLocalTZ() {
	if os.Getenv("TZ") != "" {
		return
	}
	if _, err := os.Stat("/etc/localtime"); err == nil {
		return
	}
	name := getprop("persist.sys.timezone")
	if name == "" {
		return // stay on UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		time.Local = loc
	}
}

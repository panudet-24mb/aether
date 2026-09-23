package main

import (
	"aether/backend/internal/simulation"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type settings struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Topic    string `json:"topic"`
	CA       string `json:"ca"`
	// ZoneB is an optional second virtual gateway that hears only the wearables (multi-gateway roaming).
	ZoneB *struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Topic    string `json:"topic"`
	} `json:"zone_b"`
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	// "sample [step]" prints one uplink without touching a broker, so tests and bring-up can replay the kit
	// through the HTTP ingest path.
	if len(os.Args) >= 2 && os.Args[1] == "sample" {
		step := 0
		if len(os.Args) == 3 {
			n, e := strconv.Atoi(os.Args[2])
			if e != nil || n < 0 || n > 100000 {
				return fmt.Errorf("sample step out of range")
			}
			step = n
		}
		fmt.Println(string(simulation.Packet(step, time.Now().UTC())))
		return nil
	}
	// "sample-mos <step>" prints one MG4-style uplink of the MOS smart-office kit (ISO-8601 timestamps).
	if len(os.Args) >= 2 && os.Args[1] == "sample-mos" {
		step := 0
		if len(os.Args) == 3 {
			n, e := strconv.Atoi(os.Args[2])
			if e != nil || n < 0 || n > 100000 {
				return fmt.Errorf("sample step out of range")
			}
			step = n
		}
		fmt.Println(string(simulation.MOSPacket(step, time.Now().UTC())))
		return nil
	}
	// SIMULATOR_KIT=mos publishes the MOS smart-office kit (MG4, S1, MSP01, C10, MBT01, S4) instead of the MHS kit.
	packet, banner := simulation.Packet, "SIMULATION: virtual Minew MHS kit (4× S1-style sensors, C10, B7, B10, E8S, MBT01); no hardware commands"
	switch os.Getenv("SIMULATOR_KIT") {
	case "", "mhs":
	case "mos":
		packet, banner = simulation.MOSPacket, "SIMULATION: virtual Minew MOS kit via MG4 (S1, MSP01, C10, MBT01, S4 with a SYNTHETIC door frame); no hardware commands"
	default:
		return fmt.Errorf("SIMULATOR_KIT must be mhs or mos")
	}
	b, e := os.ReadFile(os.Getenv("SIMULATOR_CONFIG_FILE"))
	if e != nil {
		return fmt.Errorf("simulator config unavailable")
	}
	var cfg settings
	if json.Unmarshal(b, &cfg) != nil {
		return fmt.Errorf("invalid simulator config")
	}
	u, e := url.Parse(cfg.URL)
	if e != nil || u.Scheme != "ssl" || u.User != nil || u.Hostname() == "" || len(cfg.Password) < 32 || cfg.Topic == "" {
		return fmt.Errorf("simulator requires TLS and dedicated credentials")
	}
	ca, e := os.ReadFile(cfg.CA)
	if e != nil {
		return fmt.Errorf("CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid CA")
	}
	options := mqtt.NewClientOptions().AddBroker(cfg.URL).SetClientID("aether-simulator-v1").SetUsername(cfg.Username).SetPassword(cfg.Password).SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}).SetAutoReconnect(true).SetConnectTimeout(10 * time.Second)
	client := mqtt.NewClient(options)
	t := client.Connect()
	if !t.WaitTimeout(15*time.Second) || t.Error() != nil {
		return fmt.Errorf("simulator broker connection failed")
	}
	defer client.Disconnect(500)
	var zoneB mqtt.Client
	if cfg.ZoneB != nil {
		if len(cfg.ZoneB.Password) < 32 || cfg.ZoneB.Topic == "" || cfg.ZoneB.Username == "" {
			return fmt.Errorf("zone B requires dedicated credentials")
		}
		zoneB = mqtt.NewClient(mqtt.NewClientOptions().AddBroker(cfg.URL).SetClientID("aether-simulator-zone-b-v1").SetUsername(cfg.ZoneB.Username).SetPassword(cfg.ZoneB.Password).SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}).SetAutoReconnect(true).SetConnectTimeout(10 * time.Second))
		t := zoneB.Connect()
		if !t.WaitTimeout(15*time.Second) || t.Error() != nil {
			return fmt.Errorf("simulator zone B connection failed")
		}
		defer zoneB.Disconnect(500)
		fmt.Println("SIMULATION: zone B gateway enabled; wearables (C10, B7, B10) walk between zone A and zone B")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	fmt.Println(banner)
	for step := 0; ; step++ {
		t := client.Publish(cfg.Topic, 1, false, packet(step, time.Now().UTC()))
		if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
			return fmt.Errorf("simulator publish failed")
		}
		if zoneB != nil {
			t := zoneB.Publish(cfg.ZoneB.Topic, 1, false, simulation.ZonePacket(step, time.Now().UTC()))
			if !t.WaitTimeout(10*time.Second) || t.Error() != nil {
				return fmt.Errorf("simulator zone B publish failed")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

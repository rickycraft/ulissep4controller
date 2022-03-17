package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultPort      = 50050
	defaultAddr      = "127.0.0.1"
	defaultWait      = 250 * time.Millisecond
	reconnectTimeout = 5 * time.Second
	packetCounter    = "MyIngress.port_packets_in"
	packetCountWarn  = 20
	packetCheckRate  = 5 * time.Second
	digestName       = "digest_t"
)

var (
	maxRetry int
)

func main() {
	var nDevices int
	flag.IntVar(&nDevices, "n", 1, "Number of devices")
	flag.IntVar(&maxRetry, "retry", 0, "Number of times retry to connect")
	var verbose bool
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose mode with debug log messages")
	var trace bool
	flag.BoolVar(&trace, "trace", false, "Enable trace mode with log messages")
	var binPath string
	flag.StringVar(&binPath, "bin", "", "Path to P4 bin (not needed for bmv2 simple_switch_grpc)")
	var p4infoPath string
	flag.StringVar(&p4infoPath, "p4info", "", "Path to P4Info (not needed for bmv2 simple_switch_grpc)")
	flag.Parse()

	if verbose {
		log.SetLevel(log.DebugLevel)
	}
	if trace {
		log.SetLevel(log.TraceLevel)
	}
	log.Infof("Starting %d devices", nDevices)

	switchs := make([]*GrpcSwitch, nDevices)
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < nDevices; i++ {
		sw := createSwitch(ctx, uint64(i+1), binPath, p4infoPath, 3, "routes.json")
		if err := sw.runSwitch(); err != nil {
			sw.log.Errorf("Cannot start")
			log.Errorf("%v", err)
		}
		switchs[i] = sw
	}

	// clean exit
	//signalCh := signals.RegisterSignalHandlers()

	buff := make([]byte, 10)
	n, _ := os.Stdin.Read(buff)
	current_route := "routes.json"
	for n > 0 {
		log.Info("Changing switch config")
		if current_route == "routes.json" {
			current_route = "routes_long.json"
		} else {
			current_route = "routes.json"
		}
		for _, sw := range switchs {
			if err := sw.UpdateSwConfig(p4infoPath, current_route); err != nil {
				log.Errorf("Error updating swConfig: %v", err)
			}
		}
		log.Info("Press enter to change switch config")
		n, _ = os.Stdin.Read(buff)
	}

	fmt.Println()
	cancel()
	time.Sleep(defaultWait)
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/the-jonsey/pulseaudio"
)

type SystemCaptureManager struct {
	client *pulseaudio.Client

	hardwareSinkName string
	captureSinkName  string

	nullModuleIdx uint32
	loopModuleIdx uint32

	bypassApps      map[string]struct{}
	onlyCaptureApps map[string]struct{}
	monitorApps     map[string]struct{}

	movedMonitorOutputs map[uint32]struct{}

	pactlPath       string
	defaultSinkMode bool
	noLoopback      bool

	closeOnce sync.Once
}

func parseApps(value string) []string {
	if value == "" {
		return nil
	}

	var apps []string

	for app := range strings.SplitSeq(value, ",") {
		app = strings.TrimSpace(app)
		if app != "" {
			apps = append(apps, app)
		}
	}

	return apps
}

func buildAppSet(apps []string) map[string]struct{} {
	set := make(map[string]struct{}, len(apps))

	for _, app := range apps {
		app = strings.TrimSpace(strings.ToLower(app))
		if app != "" {
			set[app] = struct{}{}
		}
	}

	return set
}

func mapKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))

	for key := range set {
		keys = append(keys, key)
	}

	return keys
}

func NewSystemCaptureManager(
	captureSinkName string,
	defaultSinkMode bool,
	noLoopback bool,
	bypassApps []string,
	onlyCaptureApps []string,
	monitorApps []string,
) (*SystemCaptureManager, error) {
	if captureSinkName == "" {
		return nil, errors.New("capture sink name cannot be empty")
	}

	pactlPath, err := exec.LookPath("pactl")
	if err != nil {
		return nil, fmt.Errorf("pactl is required: %w", err)
	}

	client, err := pulseaudio.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to PulseAudio: %w", err)
	}

	origSink, err := client.GetDefaultSink()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to get default sink: %w", err)
	}

	if strings.EqualFold(origSink.Name, captureSinkName) {
		client.Close()
		return nil, fmt.Errorf(
			"original default sink is already %q",
			captureSinkName,
		)
	}

	nullArgs := fmt.Sprintf(
		"sink_name=%s rate=48000 channels=2 sink_properties=device.description=\"%s\"",
		captureSinkName,
		captureSinkName,
	)

	nullModuleIdx, err := client.LoadModule("module-null-sink", nullArgs)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to load module-null-sink: %w", err)
	}

	manager := &SystemCaptureManager{
		client:              client,
		hardwareSinkName:    origSink.Name,
		captureSinkName:     captureSinkName,
		nullModuleIdx:       nullModuleIdx,
		bypassApps:          buildAppSet(bypassApps),
		onlyCaptureApps:     buildAppSet(onlyCaptureApps),
		monitorApps:         buildAppSet(monitorApps),
		movedMonitorOutputs: make(map[uint32]struct{}),
		pactlPath:           pactlPath,
		defaultSinkMode:     defaultSinkMode,
		noLoopback:          noLoopback,
	}

	if err := manager.initialize(); err != nil {
		manager.Close()
		return nil, err
	}

	return manager, nil
}

func (m *SystemCaptureManager) initialize() error {
	if err := m.client.SetSinkVolume(m.captureSinkName, 1.0); err != nil {
		return fmt.Errorf("failed to set capture sink volume: %w", err)
	}

	if m.defaultSinkMode && len(m.onlyCaptureApps) == 0 {
		if err := m.client.SetDefaultSink(m.captureSinkName); err != nil {
			return fmt.Errorf("failed to set default sink: %w", err)
		}
	}

	if err := m.createLoopback(); err != nil {
		return err
	}

	m.cleanupExistingModules()

	if err := m.enforceRouting(); err != nil {
		return fmt.Errorf("failed to route streams: %w", err)
	}

	if err := m.EnforceMonitorRouting(); err != nil {
		return fmt.Errorf("failed to route monitor streams: %w", err)
	}

	return nil
}

func (m *SystemCaptureManager) createLoopback() error {
	if m.noLoopback {
		return nil
	}

	loopArgs := fmt.Sprintf(
		"source=%s.monitor sink=%s adjust_time=1",
		m.captureSinkName,
		m.hardwareSinkName,
	)

	moduleIdx, err := m.client.LoadModule("module-loopback", loopArgs)
	if err != nil {
		return fmt.Errorf("failed to load module-loopback: %w", err)
	}

	m.loopModuleIdx = moduleIdx
	return nil
}

func (m *SystemCaptureManager) recreateLoopback() error {
	if m.loopModuleIdx != 0 {
		if err := m.client.UnloadModule(m.loopModuleIdx); err != nil {
			return fmt.Errorf("failed to unload old loopback: %w", err)
		}

		m.loopModuleIdx = 0
	}

	return m.createLoopback()
}

func (m *SystemCaptureManager) handleUpdate() {
	if err := m.syncHardwareOutput(); err != nil {
		fmt.Printf("Hardware output sync failed: %v\n", err)
		return // Bail out early if hardware sync failed
	}

	// Verify our hardware sink is actually valid right now before routing
	if _, err := m.getSink(m.hardwareSinkName); err != nil {
		return // Sink is mid-transition, skip this tick safely
	}

	if err := m.enforceRouting(); err != nil {
		fmt.Printf("Routing enforcement failed: %v\n", err)
	}

	if err := m.EnforceMonitorRouting(); err != nil {
		fmt.Printf("Monitor routing failed: %v\n", err)
	}
}

func (m *SystemCaptureManager) Listen() error {
	updates, err := m.client.UpdatesByType(
		pulseaudio.SUBSCRIPTION_MASK_SINK |
			pulseaudio.SUBSCRIPTION_MASK_SINK_INPUT |
			pulseaudio.SUBSCRIPTION_MASK_SOURCE_OUTPUT,
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe to audio updates: %w", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	fmt.Println("System capture active.")
	fmt.Printf("Physical output: %s\n", m.hardwareSinkName)
	fmt.Printf("Capture sink: %s\n", m.captureSinkName)

	if len(m.onlyCaptureApps) > 0 {
		fmt.Printf(
			"Exclusive capture list: %s\n",
			strings.Join(mapKeys(m.onlyCaptureApps), ", "),
		)
	} else if len(m.bypassApps) > 0 {
		fmt.Printf(
			"Bypass list: %s\n",
			strings.Join(mapKeys(m.bypassApps), ", "),
		)
	}

	if len(m.monitorApps) > 0 {
		fmt.Printf(
			"Monitor list: %s\n",
			strings.Join(mapKeys(m.monitorApps), ", "),
		)
	}

	fmt.Println("Press Ctrl+C to restore audio configuration.")

	for {
		select {
		case <-updates:
			m.handleUpdate()

		case sig := <-sigChan:
			fmt.Printf("Caught signal (%v). Cleaning up...\n", sig)
			return nil
		}
	}
}

func main() {
	bypassArg := flag.String(
		"bypass",
		"",
		"Comma-separated application names to bypass capture",
	)

	onlyCaptureArg := flag.String(
		"onlyCapture",
		"",
		"Comma-separated application names to exclusively capture into the capture sink",
	)

	monitorArg := flag.String(
		"monitor",
		"",
		"Comma-separated application names whose recordings should use the capture sink monitor",
	)

	defaultSinkMode := flag.Bool(
		"defaultSinkMode",
		false,
		"Set the virtual sink as default sink. May help with some apps, but can cause issues with volume control.",
	)

	noLoopback := flag.Bool(
		"noLoopback",
		false,
		"Disable loopback. Any app in the virtual sink will not be played back to the main speaker (except bypassed apps)",
	)

	flag.Parse()

	manager, err := NewSystemCaptureManager(
		"SystemCaptureSink",
		*defaultSinkMode,
		*noLoopback,
		parseApps(*bypassArg),
		parseApps(*onlyCaptureArg),
		parseApps(*monitorArg),
	)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	defer manager.Close()

	if err := manager.Listen(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

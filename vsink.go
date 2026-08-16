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
	"time"

	"github.com/the-jonsey/pulseaudio"
)

type SystemCaptureManager struct {
	client *pulseaudio.Client

	hardwareSinkName       string
	restoreDefaultSinkName string
	captureSinkName        string

	nullModuleIdx uint32
	loopModuleIdx uint32

	bypassApps  map[string]struct{}
	monitorApps map[string]struct{}

	movedMonitorOutputs map[uint32]struct{}

	pactlPath string

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

func NewSystemCaptureManager(
	captureSinkName string,
	bypassApps []string,
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
		client:                 client,
		hardwareSinkName:       origSink.Name,
		restoreDefaultSinkName: origSink.Name,
		captureSinkName:        captureSinkName,
		nullModuleIdx:          nullModuleIdx,
		bypassApps:             buildAppSet(bypassApps),
		monitorApps:            buildAppSet(monitorApps),
		movedMonitorOutputs:    make(map[uint32]struct{}),
		pactlPath:              pactlPath,
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

	if err := m.client.SetDefaultSink(m.captureSinkName); err != nil {
		return fmt.Errorf("failed to set default sink: %w", err)
	}

	if err := m.routeExistingStreams(); err != nil {
		return fmt.Errorf("failed to route existing streams: %w", err)
	}

	if err := m.EnforceMonitorRouting(); err != nil {
		return fmt.Errorf("failed to route existing monitor streams: %w", err)
	}

	if err := m.createLoopback(); err != nil {
		return err
	}

	if err := m.EnforceMonitorRouting(); err != nil {
		return fmt.Errorf(
			"failed to route monitor streams after loopback creation: %w",
			err,
		)
	}

	return nil
}

func (m *SystemCaptureManager) createLoopback() error {
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

func (m *SystemCaptureManager) isBypassed(input pulseaudio.SinkInput) bool {
	return m.matchesApp(input.PropList, m.bypassApps)
}

func (m *SystemCaptureManager) isMonitoredApp(output pulseaudio.SourceOutput) bool {
	return m.matchesApp(output.PropList, m.monitorApps)
}

func (m *SystemCaptureManager) matchesApp(
	props map[string]string,
	apps map[string]struct{},
) bool {
	keys := []string{
		"application.name",
		"application.process.binary",
		"application.process.executable",
	}

	for _, key := range keys {
		value := strings.TrimSpace(strings.ToLower(props[key]))
		if value == "" {
			continue
		}

		if _, ok := apps[value]; ok {
			return true
		}
	}

	return false
}

func (m *SystemCaptureManager) getSink(name string) (pulseaudio.Sink, error) {
	sinks, err := m.client.Sinks()
	if err != nil {
		return pulseaudio.Sink{}, fmt.Errorf(
			"failed to enumerate sinks: %w",
			err,
		)
	}

	for _, sink := range sinks {
		if sink.Name == name {
			return sink, nil
		}
	}

	return pulseaudio.Sink{}, fmt.Errorf("sink %q not found", name)
}

func (m *SystemCaptureManager) getSinkID(name string) (uint32, error) {
	sink, err := m.getSink(name)
	if err != nil {
		return 0, err
	}

	return sink.Index, nil
}

func (m *SystemCaptureManager) moveSinkInput(
	inputIndex uint32,
	sinkIndex uint32,
) error {
	cmd := exec.Command(
		m.pactlPath,
		"move-sink-input",
		fmt.Sprintf("%d", inputIndex),
		fmt.Sprintf("%d", sinkIndex),
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"move-sink-input %d -> %d failed: %w: %s",
			inputIndex,
			sinkIndex,
			err,
			strings.TrimSpace(string(output)),
		)
	}

	return nil
}

func (m *SystemCaptureManager) moveSourceOutput(
	outputIndex uint32,
	sourceName string,
) error {
	cmd := exec.Command(
		m.pactlPath,
		"move-source-output",
		fmt.Sprintf("%d", outputIndex),
		sourceName,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"move-source-output %d -> %q failed: %w: %s",
			outputIndex,
			sourceName,
			err,
			strings.TrimSpace(string(output)),
		)
	}

	return nil
}

func appName(props map[string]string, fallback string) string {
	for _, key := range []string{
		"application.name",
		"application.process.binary",
	} {
		if value := props[key]; value != "" {
			return value
		}
	}

	return fallback
}

func (m *SystemCaptureManager) routeExistingStreams() error {
	hardwareSinkID, err := m.getSinkID(m.hardwareSinkName)
	if err != nil {
		return err
	}

	captureSinkID, err := m.getSinkID(m.captureSinkName)
	if err != nil {
		return err
	}

	inputs, err := m.client.SinkInputs()
	if err != nil {
		return fmt.Errorf("failed to enumerate sink inputs: %w", err)
	}

	var firstErr error

	for _, input := range inputs {
		if input.OwnerModule == m.loopModuleIdx {
			continue
		}

		targetID := captureSinkID
		targetName := m.captureSinkName

		if m.isBypassed(input) {
			targetID = hardwareSinkID
			targetName = m.hardwareSinkName
		}

		if input.Sink == targetID {
			continue
		}

		name := appName(input.PropList, input.Name)

		if err := m.moveSinkInput(input.Index, targetID); err != nil {
			fmt.Printf(
				"Failed to route %q (%d) -> %q: %v\n",
				name,
				input.Index,
				targetName,
				err,
			)

			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		fmt.Printf(
			"%s -> %s (%d)\n",
			name,
			targetName,
			input.Index,
		)
	}

	return firstErr
}

func (m *SystemCaptureManager) enforceBypass() error {
	hardwareSinkID, err := m.getSinkID(m.hardwareSinkName)
	if err != nil {
		return err
	}

	inputs, err := m.client.SinkInputs()
	if err != nil {
		return fmt.Errorf("failed to enumerate sink inputs: %w", err)
	}

	var firstErr error

	for _, input := range inputs {
		if input.OwnerModule == m.loopModuleIdx {
			continue
		}

		if !m.isBypassed(input) {
			continue
		}

		if input.Sink == hardwareSinkID {
			continue
		}

		name := appName(input.PropList, input.Name)

		if err := m.moveSinkInput(input.Index, hardwareSinkID); err != nil {
			fmt.Printf(
				"Failed to bypass %q (%d): %v\n",
				name,
				input.Index,
				err,
			)

			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		fmt.Printf(
			"Bypassed %s (%d)\n",
			name,
			input.Index,
		)
	}

	return firstErr
}

func (m *SystemCaptureManager) EnforceMonitorRouting() error {
	hardwareSink, err := m.getSink(m.hardwareSinkName)
	if err != nil {
		return err
	}

	captureSink, err := m.getSink(m.captureSinkName)
	if err != nil {
		return err
	}

	outputs, err := m.client.SourceOutputs()
	if err != nil {
		return fmt.Errorf("failed to enumerate source outputs: %w", err)
	}

	var firstErr error

	for _, output := range outputs {
		if output.OwnerModule == m.loopModuleIdx {
			continue
		}

		if !m.isMonitoredApp(output) {
			continue
		}

		if output.Source != hardwareSink.MonitorSourceIndex {
			continue
		}

		name := appName(output.PropList, output.Name)

		if err := m.moveSourceOutput(
			output.Index,
			captureSink.MonitorSourceName,
		); err != nil {
			fmt.Printf(
				"Failed to route recording stream %q (%d): %v\n",
				name,
				output.Index,
				err,
			)

			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		m.movedMonitorOutputs[output.Index] = struct{}{}

		fmt.Printf(
			"%s -> %s (%d)\n",
			name,
			captureSink.MonitorSourceName,
			output.Index,
		)
	}

	return firstErr
}

func (m *SystemCaptureManager) getBestHardwareSink() (string, error) {
	sinks, err := m.client.Sinks()
	if err != nil {
		return "", fmt.Errorf("failed to enumerate sinks: %w", err)
	}

	// Build a map of currently active, real, and PLUGGED IN sinks
	validSinks := make(map[string]bool)

	for _, s := range sinks {
		if s.Name == m.captureSinkName {
			continue
		}

		// Check if the sink is physically unplugged
		isUnplugged := false

		if s.ActivePortName != "" {
			for _, port := range s.Ports {
				if port.Name == s.ActivePortName {
					// PulseAudio port availability:
					// 0 = Unknown, 1 = No (Unplugged), 2 = Yes (Plugged in)
					if port.Available == 1 {
						isUnplugged = true
					}
					break
				}
			}
		}

		if !isUnplugged {
			validSinks[s.Name] = true
		}
	}

	if len(validSinks) == 0 {
		return "", errors.New("no physical hardware sinks available or plugged in")
	}

	// 1. Did the OS default point to a REAL, currently plugged-in sink?
	defaultSink, err := m.client.GetDefaultSink()
	if err == nil && defaultSink.Name != "" && defaultSink.Name != m.captureSinkName {
		if validSinks[defaultSink.Name] {
			return defaultSink.Name, nil
		}
	}

	// 2. Is our current hardware sink still perfectly fine and plugged in? DO NOT SWITCH.
	if m.hardwareSinkName != "" && validSinks[m.hardwareSinkName] {
		return m.hardwareSinkName, nil
	}

	// 3. Fallback: Our current sink was unplugged, and OS default is useless.
	// Just pick the first sink that is physically plugged in (or unknown state).
	var fallback string
	for name := range validSinks {
		fallback = name
		// If we wanted to prioritize certain sink priorities we could do it here,
		// but taking the first available valid sink is usually enough.
		break
	}

	return fallback, nil
}

func (m *SystemCaptureManager) syncHardwareOutput() error {
	newHardwareSink, err := m.getBestHardwareSink()
	if err != nil {
		// Nothing valid to switch to, just wait.
		return nil
	}

	// If the best sink is already our current sink, do absolutely nothing.
	if newHardwareSink == m.hardwareSinkName {
		return nil
	}

	fmt.Printf(
		"Hardware output changed: %s -> %s\n",
		m.hardwareSinkName,
		newHardwareSink,
	)

	m.hardwareSinkName = newHardwareSink

	if err := m.recreateLoopback(); err != nil {
		return fmt.Errorf("failed to retarget loopback: %w", err)
	}

	if err := m.client.SetDefaultSink(m.captureSinkName); err != nil {
		return fmt.Errorf("failed to restore capture sink as default: %w", err)
	}

	if err := m.enforceBypass(); err != nil {
		return fmt.Errorf("failed to update bypass routing: %w", err)
	}

	if err := m.EnforceMonitorRouting(); err != nil {
		return fmt.Errorf("failed to update monitor routing: %w", err)
	}

	return nil
}
func (m *SystemCaptureManager) handleUpdate() {
	if err := m.syncHardwareOutput(); err != nil {
		fmt.Printf("Hardware output sync failed: %v\n", err)
	}

	if err := m.enforceBypass(); err != nil {
		fmt.Printf("Bypass routing failed: %v\n", err)
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

	if len(m.bypassApps) > 0 {
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

func mapKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))

	for key := range set {
		keys = append(keys, key)
	}

	return keys
}

func (m *SystemCaptureManager) restoreStreams() error {
	hardwareSinkID, err := m.getSinkID(m.hardwareSinkName)
	if err != nil {
		return err
	}

	captureSinkID, err := m.getSinkID(m.captureSinkName)
	if err != nil {
		return err
	}

	inputs, err := m.client.SinkInputs()
	if err != nil {
		return fmt.Errorf("failed to enumerate sink inputs: %w", err)
	}

	var firstErr error

	for _, input := range inputs {
		if input.OwnerModule == m.loopModuleIdx {
			continue
		}

		if input.Sink != captureSinkID {
			continue
		}

		name := appName(input.PropList, input.Name)

		if err := m.moveSinkInput(input.Index, hardwareSinkID); err != nil {
			fmt.Printf(
				"Failed to restore playback stream %q (%d): %v\n",
				name,
				input.Index,
				err,
			)

			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		fmt.Printf(
			"Restored playback stream %q (%d)\n",
			name,
			input.Index,
		)
	}

	return firstErr
}

func (m *SystemCaptureManager) restoreSourceOutputs() error {
	hardwareSink, err := m.getSink(m.hardwareSinkName)
	if err != nil {
		return err
	}

	outputs, err := m.client.SourceOutputs()
	if err != nil {
		return fmt.Errorf("failed to enumerate source outputs: %w", err)
	}

	var firstErr error

	for _, output := range outputs {
		if _, ok := m.movedMonitorOutputs[output.Index]; !ok {
			continue
		}

		name := appName(output.PropList, output.Name)

		if err := m.moveSourceOutput(
			output.Index,
			hardwareSink.MonitorSourceName,
		); err != nil {
			fmt.Printf(
				"Failed to restore recording stream %q (%d): %v\n",
				name,
				output.Index,
				err,
			)

			if firstErr == nil {
				firstErr = err
			}

			continue
		}

		delete(m.movedMonitorOutputs, output.Index)

		fmt.Printf(
			"Restored recording stream %q (%d)\n",
			name,
			output.Index,
		)
	}

	return firstErr
}

func (m *SystemCaptureManager) monitorOutputsRestored() bool {
	if len(m.movedMonitorOutputs) == 0 {
		return true
	}

	outputs, err := m.client.SourceOutputs()
	if err != nil {
		return false
	}

	live := make(map[uint32]struct{}, len(outputs))

	for _, output := range outputs {
		live[output.Index] = struct{}{}
	}

	for index := range m.movedMonitorOutputs {
		if _, ok := live[index]; !ok {
			delete(m.movedMonitorOutputs, index)
		}
	}

	return len(m.movedMonitorOutputs) == 0
}

func (m *SystemCaptureManager) waitForStreamsToLeaveCapture() {
	const (
		interval = 20 * time.Millisecond
		timeout  = 2 * time.Second
	)

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		captureSink, err := m.getSink(m.captureSinkName)
		if err != nil {
			return
		}

		inputs, err := m.client.SinkInputs()
		if err != nil {
			return
		}

		remainingInputs := 0

		for _, input := range inputs {
			if input.OwnerModule == m.loopModuleIdx {
				continue
			}

			if input.Sink == captureSink.Index {
				remainingInputs++
			}
		}

		if remainingInputs == 0 && m.monitorOutputsRestored() {
			fmt.Println("All application streams restored.")
			return
		}

		time.Sleep(interval)
	}

	fmt.Println(
		"Timed out waiting for application streams to leave capture graph.",
	)
}

func (m *SystemCaptureManager) cleanupModules() {
	if m.loopModuleIdx != 0 {
		if err := m.client.UnloadModule(m.loopModuleIdx); err != nil {
			fmt.Printf(
				"Failed to unload loopback module %d: %v\n",
				m.loopModuleIdx,
				err,
			)
		}

		m.loopModuleIdx = 0
	}

	if m.nullModuleIdx != 0 {
		if err := m.client.UnloadModule(m.nullModuleIdx); err != nil {
			fmt.Printf(
				"Failed to unload virtual sink module %d: %v\n",
				m.nullModuleIdx,
				err,
			)
		}

		m.nullModuleIdx = 0
	}
}

func (m *SystemCaptureManager) Close() {
	m.closeOnce.Do(func() {
		if m.client == nil {
			return
		}

		fmt.Println("Shutting down system capture...")

		if m.hardwareSinkName != "" {
			if err := m.client.SetDefaultSink(m.hardwareSinkName); err != nil {
				fmt.Printf(
					"Failed to restore default sink: %v\n",
					err,
				)
			}
		}

		if err := m.restoreStreams(); err != nil {
			fmt.Printf(
				"Some playback streams could not be restored: %v\n",
				err,
			)
		}

		if err := m.restoreSourceOutputs(); err != nil {
			fmt.Printf(
				"Some recording streams could not be restored: %v\n",
				err,
			)
		}

		m.waitForStreamsToLeaveCapture()
		m.cleanupModules()
		m.client.Close()

		fmt.Println("Audio configuration restored.")
	})
}

func main() {
	bypassArg := flag.String(
		"bypass",
		"",
		"Comma-separated application names to bypass capture",
	)

	monitorArg := flag.String(
		"monitor",
		"",
		"Comma-separated application names whose recordings should use the capture sink monitor",
	)

	flag.Parse()

	manager, err := NewSystemCaptureManager(
		"SystemCaptureSink",
		parseApps(*bypassArg),
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

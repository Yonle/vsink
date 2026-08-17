package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/the-jonsey/pulseaudio"
)

func (m *SystemCaptureManager) isBypassed(input pulseaudio.SinkInput) bool {
	return m.matchesApp(input.PropList, m.bypassApps)
}

func (m *SystemCaptureManager) isOnlyCaptured(input pulseaudio.SinkInput) bool {
	return m.matchesApp(input.PropList, m.onlyCaptureApps)
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

func (m *SystemCaptureManager) getSinks() ([]pulseaudio.Sink, error) {
	return m.client.Sinks()
}

func (m *SystemCaptureManager) getSink(name string) (pulseaudio.Sink, error) {
	sinks, err := m.getSinks()
	if err != nil {
		return pulseaudio.Sink{}, fmt.Errorf("failed to enumerate sinks: %w", err)
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

func (m *SystemCaptureManager) enforceRouting() error {
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

		var targetID uint32
		var targetName string

		if len(m.onlyCaptureApps) > 0 {
			if m.isOnlyCaptured(input) {
				targetID = captureSinkID
				targetName = m.captureSinkName
			} else {
				targetID = hardwareSinkID
				targetName = m.hardwareSinkName
			}
		} else {
			if m.isBypassed(input) {
				targetID = hardwareSinkID
				targetName = m.hardwareSinkName
			} else {
				targetID = captureSinkID
				targetName = m.captureSinkName
			}
		}

		if input.Sink == targetID {
			continue
		}

		name := appName(input.PropList, input.Name)

		if err := m.moveSinkInput(input.Index, targetID); err != nil {
			fmt.Printf(
				"Failed to adjust routing for %q (%d): %v\n",
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
			"Routed %s -> %s (%d)\n",
			name,
			targetName,
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
	sinks, err := m.getSinks()
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

	if m.defaultSinkMode && len(m.onlyCaptureApps) == 0 {
		if err := m.client.SetDefaultSink(m.captureSinkName); err != nil {
			return fmt.Errorf("failed to restore capture sink as default: %w", err)
		}
	}

	return nil
}

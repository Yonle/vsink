package main

import (
	"fmt"
	"strings"
	"time"
)

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

func (m *SystemCaptureManager) cleanupExistingModules() error {
	modules, err := m.client.ModuleList()
	if err != nil {
		return fmt.Errorf("failed to enumerate modules: %w", err)
	}

	// Remove loopbacks first.
	for _, module := range modules {
		if module.Name != "module-loopback" {
			continue
		}

		if !strings.Contains(module.Argument, "source="+m.captureSinkName+".monitor") {
			continue
		}

		if module.Index == m.loopModuleIdx {
			continue
		}

		if err := m.client.UnloadModule(module.Index); err != nil {
			return fmt.Errorf(
				"failed to unload existing loopback module %d: %w",
				module.Index,
				err,
			)
		}
	}

	// Then remove the virtual sink.
	for _, module := range modules {
		if module.Name != "module-null-sink" {
			continue
		}

		if !strings.Contains(module.Argument, "sink_name="+m.captureSinkName) {
			continue
		}

		if module.Index == m.nullModuleIdx {
			continue
		}

		if err := m.client.UnloadModule(module.Index); err != nil {
			return fmt.Errorf(
				"failed to unload existing null sink module %d: %w",
				module.Index,
				err,
			)
		}
	}

	return nil
}

func (m *SystemCaptureManager) Close() {
	m.closeOnce.Do(func() {
		if m.client == nil {
			return
		}

		fmt.Println("Shutting down system capture...")

		// Only restore default sink if we changed it in the first place
		if m.defaultSinkMode && len(m.onlyCaptureApps) == 0 && m.hardwareSinkName != "" {
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

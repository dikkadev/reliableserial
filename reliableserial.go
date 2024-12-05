package reliableserial

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"go.bug.st/serial"
	"golang.org/x/exp/slog"
)

// Serializable defines the methods required for data serialization and deserialization.
type Serializable interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

// DeviceMatcher defines a method to match devices based on DeviceInfo.
type DeviceMatcher interface {
	Match(deviceInfo DeviceInfo) bool
}

// DeviceInfo holds information about the serial device.
type DeviceInfo struct {
	Name string
	ID   string
}

// SerialConfig holds the serial port configuration.
type SerialConfig struct {
	BaudRate int
}

// ReliableSerial manages reliable communication over a serial port.
type ReliableSerial struct {
	sendCh    chan Serializable
	receiveCh chan Serializable

	deviceMatcher DeviceMatcher
	serialConfig  SerialConfig
	logger        *slog.Logger

	deviceConnected chan DeviceInfo

	serialPort io.ReadWriteCloser

	// Internal synchronization
	mu               sync.Mutex
	isRunning        bool
	ctx              context.Context
	cancel           context.CancelFunc
	deviceCancel     context.CancelFunc
	deviceCancelOnce sync.Once

	receiveBuffer       []byte
	delimiter           byte
	serializableFactory func() Serializable

	serialPortOpener func(name string, mode *serial.Mode) (io.ReadWriteCloser, error)
}

// NewReliableSerial creates a new ReliableSerial instance.
func NewReliableSerial(
	deviceMatcher DeviceMatcher,
	serialConfig SerialConfig,
	logger *slog.Logger,
	delimiter byte,
	serializableFactory func() Serializable,
	serialPortOpener func(name string, mode *serial.Mode) (io.ReadWriteCloser, error),
) *ReliableSerial {
	ctx, cancel := context.WithCancel(context.Background())
	rs := &ReliableSerial{
		sendCh:    make(chan Serializable),
		receiveCh: make(chan Serializable),

		deviceMatcher: deviceMatcher,
		serialConfig:  serialConfig,
		logger:        logger,

		deviceConnected: make(chan DeviceInfo),

		ctx:    ctx,
		cancel: cancel,

		delimiter:           delimiter,
		serializableFactory: serializableFactory,

		serialPortOpener: serialPortOpener,
	}

	go rs.runDeviceMonitor()
	go rs.runCommunication()

	return rs
}

// SendChannel returns the send channel for sending data.
func (rs *ReliableSerial) SendChannel() chan<- Serializable {
	return rs.sendCh
}

// ReceiveChannel returns the receive channel for receiving data.
func (rs *ReliableSerial) ReceiveChannel() <-chan Serializable {
	return rs.receiveCh
}

// Close stops all operations and closes the serial port.
func (rs *ReliableSerial) Close() {
	rs.cancel()
}

// IsRunning returns true if the serial communication is active.
func (rs *ReliableSerial) IsRunning() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.isRunning
}

// runDeviceMonitor monitors for connected devices matching the DeviceMatcher.
func (rs *ReliableSerial) runDeviceMonitor() {
	rs.logger.Info("Starting device monitor")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-rs.ctx.Done():
			rs.logger.Info("Device monitor stopping")
			return
		case <-ticker.C:
			rs.mu.Lock()
			isRunning := rs.isRunning
			rs.mu.Unlock()

			if isRunning {
				continue
			}

			ports, err := serial.GetPortsList()
			if err != nil {
				rs.logger.Error("Failed to list serial ports", "error", err)
				continue
			}

			for _, portName := range ports {
				deviceInfo := DeviceInfo{
					Name: portName,
					ID:   portName,
				}

				if rs.deviceMatcher.Match(deviceInfo) {
					rs.logger.Info("Device matched", "device", deviceInfo)
					select {
					case rs.deviceConnected <- deviceInfo:
						// Sent device info to the channel
					default:
						// Channel is full; ignore
					}
					break
				}
			}
		}
	}
}

// runCommunication handles device connections and reconnections.
func (rs *ReliableSerial) runCommunication() {
	for {
		select {
		case <-rs.ctx.Done():
			return
		case deviceInfo := <-rs.deviceConnected:
			rs.handleDeviceConnection(deviceInfo)
		}
	}
}

// handleDeviceConnection manages the serial port connection and communication loops.
func (rs *ReliableSerial) handleDeviceConnection(deviceInfo DeviceInfo) {
	rs.logger.Info("Connecting to device", "device", deviceInfo)

	mode := &serial.Mode{
		BaudRate: rs.serialConfig.BaudRate,
	}

	port, err := rs.serialPortOpener(deviceInfo.Name, mode)
	if err != nil {
		rs.logger.Error("Failed to open serial port", "error", err)
		return
	}

	rs.serialPort = port

	// Update isRunning flag
	rs.mu.Lock()
	rs.isRunning = true
	rs.mu.Unlock()

	// Create a child context that can be canceled when the device disconnects
	deviceCtx, deviceCancel := context.WithCancel(rs.ctx)
	rs.deviceCancel = deviceCancel
	rs.deviceCancelOnce = sync.Once{}

	var wg sync.WaitGroup
	wg.Add(2)

	// Start send and receive loops
	go func() {
		defer wg.Done()
		rs.sendLoop(deviceCtx)
	}()

	go func() {
		defer wg.Done()
		rs.receiveLoop(deviceCtx)
	}()

	// Wait for sendLoop and receiveLoop to exit
	wg.Wait()

	// Clean up
	rs.mu.Lock()
	rs.isRunning = false
	rs.mu.Unlock()

	rs.deviceCancel = nil
	rs.deviceCancelOnce = sync.Once{}

	rs.serialPort.Close()
	rs.serialPort = nil

	deviceCancel()

	rs.logger.Info("Device disconnected", "device", deviceInfo)
}

// sendLoop reads from send channel, serializes data, and writes to the device.
func (rs *ReliableSerial) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-rs.sendCh:
			serializedData, err := data.Serialize()
			if err != nil {
				rs.logger.Error("Serialization error", "error", err)
				continue
			}
			// Append delimiter
			serializedData = append(serializedData, rs.delimiter)
			_, err = rs.serialPort.Write(serializedData)
			if err != nil {
				rs.logger.Error("Failed to write to serial port", "error", err)
				rs.deviceCancelOnce.Do(rs.deviceCancel)
				return
			}
		}
	}
}

// receiveLoop reads from the device, deserializes data, and sends to receive channel.
func (rs *ReliableSerial) receiveLoop(ctx context.Context) {
	buffer := make([]byte, 1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, err := rs.serialPort.Read(buffer)
			if err != nil {
				rs.logger.Error("Failed to read from serial port", "error", err)
				rs.deviceCancelOnce.Do(rs.deviceCancel)
				return
			}
			if n > 0 {
				data := buffer[:n]
				rs.handleReceivedData(data)
			}
		}
	}
}

// handleReceivedData processes incoming data and deserializes complete messages.
func (rs *ReliableSerial) handleReceivedData(data []byte) {
	rs.mu.Lock()
	rs.receiveBuffer = append(rs.receiveBuffer, data...)
	rs.mu.Unlock()

	for {
		rs.mu.Lock()
		idx := bytes.IndexByte(rs.receiveBuffer, rs.delimiter)
		if idx == -1 {
			// No complete message yet
			rs.mu.Unlock()
			break
		}

		// Extract message
		messageBytes := rs.receiveBuffer[:idx]
		// Remove message from buffer
		rs.receiveBuffer = rs.receiveBuffer[idx+1:]
		rs.mu.Unlock()

		// Deserialize message
		message := rs.serializableFactory()
		err := message.Deserialize(messageBytes)
		if err != nil {
			rs.logger.Error("Failed to deserialize message", "error", err)
			continue
		}

		// Send message to receiveCh
		select {
		case rs.receiveCh <- message:
		default:
			rs.logger.Warn("Receive channel is full, dropping message")
		}
	}
}

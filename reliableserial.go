package reliableserial

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.bug.st/serial"
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
	mu           sync.Mutex
	isRunning    bool
	ctx          context.Context
	cancel       context.CancelFunc
	deviceCancel context.CancelFunc
	// deviceCancelOnce sync.Once

	receiveBuffer       []byte
	delimiterFunc       func() []byte
	serializableFactory func() Serializable

	serialPortOpener func(name string, mode *serial.Mode) (io.ReadWriteCloser, error)
}

// NewReliableSerial creates a new ReliableSerial instance.
func NewReliableSerial(
	deviceMatcher DeviceMatcher,
	serialConfig SerialConfig,
	logger *slog.Logger,
	delimiterFunc func() []byte,
	serializableFactory func() Serializable,
	opener ...func(name string, mode *serial.Mode) (io.ReadWriteCloser, error),
) *ReliableSerial {
	ctx, cancel := context.WithCancel(context.Background())
	rs := &ReliableSerial{
		sendCh:    make(chan Serializable, 64),
		receiveCh: make(chan Serializable, 64),

		deviceMatcher: deviceMatcher,
		serialConfig:  serialConfig,
		logger:        logger,

		deviceConnected: make(chan DeviceInfo),

		ctx:    ctx,
		cancel: cancel,

		delimiterFunc:       delimiterFunc,
		serializableFactory: serializableFactory,
	}

	if len(opener) > 0 && opener[0] != nil {
		rs.serialPortOpener = opener[0]
	} else {
		rs.serialPortOpener = func(name string, mode *serial.Mode) (io.ReadWriteCloser, error) {
			return serial.Open(name, mode)
		}
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
	rs.mu.Lock()
	if rs.deviceCancel != nil {
		rs.deviceCancel()
		rs.deviceCancel = nil
	}
	rs.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
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
	rs.logger.Debug("Serial port opened", "device", deviceInfo)

	rs.serialPort = port

	rs.mu.Lock()
	rs.isRunning = true
	rs.mu.Unlock()

	deviceCtx, deviceCancel := context.WithCancel(rs.ctx)

	// Wrap deviceCancel in a sync.Once to avoid multiple calls
	deviceCancelOnce := &sync.Once{}
	rs.deviceCancel = func() {
		deviceCancelOnce.Do(func() {
			deviceCancel()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		rs.sendLoop(deviceCtx)
	}()

	go func() {
		defer wg.Done()
		rs.receiveLoop(deviceCtx)
	}()

	wg.Wait()

	rs.mu.Lock()
	rs.isRunning = false
	rs.mu.Unlock()

	rs.deviceCancel = nil

	rs.serialPort.Close()
	rs.serialPort = nil

	rs.logger.Info("Device disconnected", "device", deviceInfo)
}

// handleDeviceConnection manages the serial port connection and communication loops.
// func (rs *ReliableSerial) handleDeviceConnection(deviceInfo DeviceInfo) {
// 	rs.logger.Info("Connecting to device", "device", deviceInfo)
//
// 	mode := &serial.Mode{
// 		BaudRate: rs.serialConfig.BaudRate,
// 	}
//
// 	port, err := rs.serialPortOpener(deviceInfo.Name, mode)
// 	// port, err := serial.Open(deviceInfo.Name, mode)
// 	if err != nil {
// 		rs.logger.Error("Failed to open serial port", "error", err)
// 		return
// 	}
// 	rs.logger.Debug("Serial port opened", "device", deviceInfo)
//
// 	rs.serialPort = port
//
// 	// Update isRunning flag
// 	rs.mu.Lock()
// 	rs.isRunning = true
// 	rs.mu.Unlock()
//
// 	// Create a child context that can be canceled when the device disconnects
// 	deviceCtx, deviceCancel := context.WithCancel(rs.ctx)
// 	rs.deviceCancel = deviceCancel
//
// 	var wg sync.WaitGroup
// 	wg.Add(2)
//
// 	// Start send and receive loops
// 	go func() {
// 		defer wg.Done()
// 		rs.sendLoop(deviceCtx)
// 	}()
//
// 	go func() {
// 		defer wg.Done()
// 		rs.receiveLoop(deviceCtx)
// 	}()
//
// 	// Wait for sendLoop and receiveLoop to exit
// 	wg.Wait()
//
// 	// Clean up
// 	rs.mu.Lock()
// 	rs.isRunning = false
// 	rs.mu.Unlock()
//
// 	rs.deviceCancel = nil
// 	// rs.deviceCancelOnce = sync.Once{}
//
// 	rs.serialPort.Close()
// 	rs.serialPort = nil
//
// 	deviceCancel()
//
// 	rs.logger.Info("Device disconnected", "device", deviceInfo)
// }

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
			delimiter := rs.delimiterFunc()
			serializedData = append(serializedData, delimiter...)
			_, err = rs.serialPort.Write(serializedData)
			if err != nil {
				rs.logger.Error("Failed to write to serial port", "error", err)
				// rs.deviceCancelOnce.Do(rs.deviceCancel)
				if rs.deviceCancel != nil {
					rs.deviceCancel()
					rs.deviceCancel = nil
				}
				return
			}
		}
	}
}

// func (rs *ReliableSerial) receiveLoop(ctx context.Context) {
// 	reader := bufio.NewReader(rs.serialPort)
// 	scanner := bufio.NewScanner(reader)
//
// 	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
// 		delimiter := rs.delimiterFunc()
// 		if atEOF && len(data) == 0 {
// 			return 0, nil, nil
// 		}
// 		if len(delimiter) > 0 {
// 			if idx := bytes.Index(data, delimiter); idx >= 0 {
// 				return idx + len(delimiter), data[:idx+len(delimiter)], nil
// 			}
// 		}
// 		if atEOF {
// 			return len(data), nil, io.EOF
// 		}
// 		return 0, nil, nil
// 	})
//
// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		default:
// 			if scanner.Scan() {
// 				packet := scanner.Bytes()
// 				if len(packet) > 0 {
// 					packet = bytes.TrimSuffix(packet, rs.delimiterFunc())
// 				}
// 				rs.handleReceivedData(packet)
// 			} else {
// 				err := scanner.Err()
// 				if err == io.EOF {
// 					rs.logger.Info("EOF detected, treating as device disconnection")
// 					if rs.deviceCancel != nil {
// 						rs.deviceCancel()
// 						rs.deviceCancel = nil
// 					}
// 				} else if err != nil {
// 					rs.logger.Error("Scanner error", "error", err)
// 					if rs.deviceCancel != nil {
// 						rs.deviceCancel()
// 						rs.deviceCancel = nil
// 					}
// 				}
// 				return
// 			}
// 		}
// 	}
// }

func (rs *ReliableSerial) receiveLoop(ctx context.Context) {
	reader := bufio.NewReader(rs.serialPort)
	scanner := bufio.NewScanner(reader)

	// Custom split function
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		delimiter := rs.delimiterFunc()
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if len(delimiter) > 0 {
			if idx := bytes.Index(data, delimiter); idx >= 0 {
				// Return the complete packet including the delimiter
				return idx + len(delimiter), data[:idx+len(delimiter)], nil
			}
		}
		// If at EOF, return the remaining data (incomplete packet)
		if atEOF {
			return len(data), nil, nil
		}
		// Request more data
		return 0, nil, nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if scanner.Scan() {
				packet := scanner.Bytes()
				// rs.logger.Debug("Received packet", "data", packet)
				// Strip the delimiter before handling
				if len(packet) > 0 {
					packet = bytes.TrimSuffix(packet, rs.delimiterFunc())
				}
				rs.handleReceivedData(packet)
			} else {
				// Added check:
				err := scanner.Err()
				if err == nil {
					// EOF reached
					rs.logger.Info("EOF reached, treating as device disconnection")
					if rs.deviceCancel != nil {
						rs.deviceCancel()
						rs.deviceCancel = nil
					}
				} else {
					rs.logger.Error("Scanner error", "error", err)
					if rs.deviceCancel != nil {
						rs.deviceCancel()
						rs.deviceCancel = nil
					}
				}
				return
			}
		}
	}

	// for {
	// 	select {
	// 	case <-ctx.Done():
	// 		return
	// 	default:
	// 		if scanner.Scan() {
	// 			packet := scanner.Bytes()
	// 			// Strip the delimiter before handling
	// 			if len(packet) > 0 {
	// 				packet = bytes.TrimSuffix(packet, rs.delimiterFunc())
	// 			}
	// 			rs.handleReceivedData(packet)
	// 		} else if err := scanner.Err(); err != nil {
	// 			rs.logger.Error("Scanner error", "error", err)
	// 			if rs.deviceCancel != nil {
	// 				rs.deviceCancel()
	// 			}
	// 			return
	// 		}
	// 	}
	// }

}

func (rs *ReliableSerial) handleReceivedData(data []byte) {
	if len(data) == 0 {
		rs.logger.Debug("Empty data received")
		return
	}

	// rs.logger.Debug("Processing received data", "data", hex.EncodeToString(data))

	// Deserialize the message
	message := rs.serializableFactory()
	if err := message.Deserialize(data); err != nil {
		rs.logger.Error("Failed to deserialize message", "error", err)
		return
	}

	// Send message to receive channel
	select {
	case rs.receiveCh <- message:
	default:
		rs.logger.Warn("Receive channel is full, dropping message")
	}
}

// receiveLoop reads from the device, deserializes data, and sends to receive channel.
// func (rs *ReliableSerial) receiveLoop(ctx context.Context) {
// 	reader := bufio.NewReader(rs.serialPort)
// 	scanner := bufio.NewScanner(reader)
//
// 	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
// 		delimiter := rs.delimiterFunc()
// 		if atEOF && len(data) == 0 {
// 			return 0, nil, nil
// 		}
// 		if len(delimiter) > 0 {
// 			if idx := bytes.Index(data, delimiter); idx >= 0 {
// 				// Return everything before the delimiter as the token
// 				return idx + len(delimiter), data[:idx], nil
// 			}
// 		}
//
// 		// If at EOF, return all remaining data
// 		if atEOF {
// 			return len(data), data, nil
// 		}
//
// 		// Need more data
// 		return 0, nil, nil
// 	})
//
// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		default:
// 			if scanner.Scan() {
// 				messageBytes := scanner.Bytes()
// 				rs.handleReceivedData(messageBytes)
// 			} else if err := scanner.Err(); err != nil {
// 				rs.logger.Error("Scanner error", "error", err)
// 				// Handle error appropriately, e.g., reconnect or cancel device
// 				rs.deviceCancel()
// 				return
// 			}
// 		}
// 	}
// }
//
// const maxReceiveBufferSize = 1024 * 1024
//
// // const maxReceiveBufferSize = 12
//
// // handleReceivedData processes incoming data and deserializes complete messages.
// func (rs *ReliableSerial) handleReceivedData(data []byte) {
// 	rs.mu.Lock()
// 	rs.receiveBuffer = append(rs.receiveBuffer, data...)
// 	if len(rs.receiveBuffer) > maxReceiveBufferSize {
// 		rs.logger.Warn("Receive buffer is full, clearing buffer")
// 		rs.receiveBuffer = nil
// 		rs.mu.Unlock()
// 		return
// 	}
// 	rs.mu.Unlock()
//
// 	sb := strings.Builder{}
// 	sb.WriteString("\n")
// 	for i, b := range rs.receiveBuffer {
// 		// sb.WriteString(fmt.Sprintf("%02X ", b))
// 		// sb.WriteString(fmt.Sprintf("%c", b))
// 		// sb.WriteString(hex.EncodeToString([]byte{b}))
// 		_ = i
// 		_ = b
// 		// if i%6 == 5 {
// 		// 	sb.WriteString("\n")
// 		// }
// 	}
// 	// rs.logger.Debug("Receive buffer", "data", sb.String())
// 	rs.logger.Debug("Receive buffer", "data", hex.EncodeToString(rs.receiveBuffer))
//
// 	for {
// 		rs.mu.Lock()
// 		delimiter := rs.delimiterFunc()
// 		idx := bytes.Index(rs.receiveBuffer, delimiter)
// 		// rs.logger.Debug("Delimiter index", "index", idx)
// 		if idx == -1 {
// 			rs.logger.Debug("Delimiter not found in buffer")
// 			rs.mu.Unlock()
// 			break
// 		}
// 		rs.logger.Debug("Delimiter found in buffer", "index", idx)
//
// 		if idx+1 > len(rs.receiveBuffer) {
// 			rs.logger.Warn("Delimiter found at the end of buffer, clearing buffer")
// 			rs.receiveBuffer = nil
// 			rs.mu.Unlock()
// 			break
// 		}
//
// 		// Extract message
// 		messageBytes := rs.receiveBuffer[:idx]
// 		rs.logger.Debug("Message bytes", "data", hex.EncodeToString(messageBytes))
// 		// Remove message from buffer
// 		rs.receiveBuffer = rs.receiveBuffer[idx+1:]
// 		rs.mu.Unlock()
//
// 		// Deserialize message
// 		message := rs.serializableFactory()
// 		err := message.Deserialize(messageBytes)
// 		if err != nil {
// 			rs.logger.Error("Failed to deserialize message", "error", err)
// 			continue
// 		}
//
// 		// Send message to receiveCh
// 		select {
// 		case rs.receiveCh <- message:
// 		default:
// 			rs.logger.Warn("Receive channel is full, dropping message")
// 		}
// 	}
// }

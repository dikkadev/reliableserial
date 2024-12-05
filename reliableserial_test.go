package reliableserial

import (
	"io"
	"testing"
	"time"

	"go.bug.st/serial"
	"golang.org/x/exp/slog"
)

// MockDeviceMatcher matches devices by name.
type MockDeviceMatcher struct {
	deviceName string
}

func (mdm *MockDeviceMatcher) Match(deviceInfo DeviceInfo) bool {
	return deviceInfo.Name == mdm.deviceName
}

// MockSerializable implements the Serializable interface for testing.
type MockSerializable struct {
	Content string
}

func (ms *MockSerializable) Serialize() ([]byte, error) {
	return []byte(ms.Content), nil
}

func (ms *MockSerializable) Deserialize(data []byte) error {
	ms.Content = string(data)
	return nil
}

// MockSerialPort simulates a serial port for testing.
type MockSerialPort struct {
	readCh  chan []byte
	writeCh chan []byte
	closed  bool
}

func NewMockSerialPort() *MockSerialPort {
	return &MockSerialPort{
		readCh:  make(chan []byte, 10),
		writeCh: make(chan []byte, 10),
	}
}

func (msp *MockSerialPort) Read(p []byte) (n int, err error) {
	if msp.closed {
		return 0, io.EOF
	}
	data, ok := <-msp.readCh
	if !ok {
		return 0, io.EOF
	}
	n = copy(p, data)
	return n, nil
}

func (msp *MockSerialPort) Write(p []byte) (n int, err error) {
	if msp.closed {
		return 0, io.ErrClosedPipe
	}
	data := make([]byte, len(p))
	copy(data, p)
	msp.writeCh <- data
	return len(p), nil
}

func (msp *MockSerialPort) Close() error {
	if msp.closed {
		return io.ErrClosedPipe
	}
	msp.closed = true
	close(msp.readCh)
	close(msp.writeCh)
	return nil
}

// serialPortOpenerMock simulates opening a serial port.
func serialPortOpenerMock(portName string, mode *serial.Mode) (io.ReadWriteCloser, error) {
	return NewMockSerialPort(), nil
}

func TestReliableSerial_DeviceAlreadyConnected(t *testing.T) {
	// Create a mock serial port
	mockSerialPort := NewMockSerialPort()

	// Override the serialPortOpener to return the mock serial port
	serialPortOpener := func(name string, mode *serial.Mode) (io.ReadWriteCloser, error) {
		return mockSerialPort, nil
	}

	// Create a DeviceMatcher that matches the mock device
	deviceMatcher := &MockDeviceMatcher{
		deviceName: "COM1",
	}

	// Create a SerializableFactory
	serializableFactory := func() Serializable {
		return &MockSerializable{}
	}

	// Create the ReliableSerial instance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := NewReliableSerial(
		deviceMatcher,
		SerialConfig{BaudRate: 9600},
		logger,
		'\n',
		serializableFactory,
		serialPortOpener,
	)

	defer rs.Close()

	// Simulate device already connected
	rs.deviceConnected <- DeviceInfo{Name: "COM1", ID: "COM1"}

	// Wait for the device to connect
	time.Sleep(1 * time.Second)

	// Check if rs.IsRunning() is true
	if !rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be true")
	}

	// Send a message
	sendCh := rs.SendChannel()
	receiveCh := rs.ReceiveChannel()

	message := &MockSerializable{Content: "Hello, device!"}
	sendCh <- message

	// Simulate device receiving the message
	select {
	case data := <-mockSerialPort.writeCh:
		expected := "Hello, device!\n"
		if string(data) != expected {
			t.Errorf("Expected data '%s', got '%s'", expected, string(data))
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for data to be written to serial port")
	}

	// Simulate device sending a message
	mockSerialPort.readCh <- []byte("Hello, host!\n")

	// Check if message is received
	select {
	case msg := <-receiveCh:
		if ms, ok := msg.(*MockSerializable); ok {
			if ms.Content != "Hello, host!" {
				t.Errorf("Expected message content 'Hello, host!', got '%s'", ms.Content)
			}
		} else {
			t.Errorf("Received message of unexpected type")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for message to be received")
	}
}

func TestReliableSerial_DeviceAppearsLater(t *testing.T) {
	// Create a mock serial port
	mockSerialPort := NewMockSerialPort()

	// Override the serialPortOpener to return the mock serial port
	serialPortOpener := func(name string, mode *serial.Mode) (io.ReadWriteCloser, error) {
		return mockSerialPort, nil
	}

	// Create a DeviceMatcher that matches the mock device
	deviceMatcher := &MockDeviceMatcher{
		deviceName: "COM1",
	}

	// Create a SerializableFactory
	serializableFactory := func() Serializable {
		return &MockSerializable{}
	}

	// Create the ReliableSerial instance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := NewReliableSerial(
		deviceMatcher,
		SerialConfig{BaudRate: 9600},
		logger,
		'\n',
		serializableFactory,
		serialPortOpener,
	)

	defer rs.Close()

	// Initially, the device is not connected
	time.Sleep(500 * time.Millisecond)

	// Check if rs.IsRunning() is false
	if rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be false before device appears")
	}

	// Simulate device appearing after some time
	go func() {
		time.Sleep(500 * time.Millisecond)
		rs.deviceConnected <- DeviceInfo{Name: "COM1", ID: "COM1"}
	}()

	// Wait for the device to connect
	time.Sleep(1 * time.Second)

	// Check if rs.IsRunning() is true
	if !rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be true after device appears")
	}

	// Send a message
	sendCh := rs.SendChannel()
	receiveCh := rs.ReceiveChannel()

	message := &MockSerializable{Content: "Hello, device!"}
	sendCh <- message

	// Simulate device receiving the message
	select {
	case data := <-mockSerialPort.writeCh:
		expected := "Hello, device!\n"
		if string(data) != expected {
			t.Errorf("Expected data '%s', got '%s'", expected, string(data))
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for data to be written to serial port")
	}

	// Simulate device sending a message
	mockSerialPort.readCh <- []byte("Hello, host!\n")

	// Check if message is received
	select {
	case msg := <-receiveCh:
		if ms, ok := msg.(*MockSerializable); ok {
			if ms.Content != "Hello, host!" {
				t.Errorf("Expected message content 'Hello, host!', got '%s'", ms.Content)
			}
		} else {
			t.Errorf("Received message of unexpected type")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for message to be received")
	}
}

func TestReliableSerial_DeviceDisconnectsAndReconnects(t *testing.T) {
	// Variable to hold the current mock serial port
	var currentMockSerialPort *MockSerialPort

	// Override the serialPortOpener to return the mock serial port
	serialPortOpener := func(name string, mode *serial.Mode) (io.ReadWriteCloser, error) {
		currentMockSerialPort = NewMockSerialPort()
		return currentMockSerialPort, nil
	}

	// Create a DeviceMatcher that matches the mock device
	deviceMatcher := &MockDeviceMatcher{
		deviceName: "COM1",
	}

	// Create a SerializableFactory
	serializableFactory := func() Serializable {
		return &MockSerializable{}
	}

	// Create the ReliableSerial instance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := NewReliableSerial(
		deviceMatcher,
		SerialConfig{BaudRate: 9600},
		logger,
		'\n',
		serializableFactory,
		serialPortOpener,
	)

	defer rs.Close()

	// Simulate device connected
	rs.deviceConnected <- DeviceInfo{Name: "COM1", ID: "COM1"}

	// Wait for the device to connect
	time.Sleep(1 * time.Second)

	// Check if rs.IsRunning() is true
	if !rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be true after device connects")
	}

	// Send a message
	sendCh := rs.SendChannel()
	receiveCh := rs.ReceiveChannel()

	message := &MockSerializable{Content: "Hello, device!"}
	sendCh <- message

	// Simulate device receiving the message
	select {
	case data := <-currentMockSerialPort.writeCh:
		expected := "Hello, device!\n"
		if string(data) != expected {
			t.Errorf("Expected data '%s', got '%s'", expected, string(data))
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for data to be written to serial port")
	}

	// Simulate device sending a message
	currentMockSerialPort.readCh <- []byte("Hello, host!\n")

	// Check if message is received
	select {
	case msg := <-receiveCh:
		if ms, ok := msg.(*MockSerializable); ok {
			if ms.Content != "Hello, host!" {
				t.Errorf("Expected message content 'Hello, host!', got '%s'", ms.Content)
			}
		} else {
			t.Errorf("Received message of unexpected type")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for message to be received")
	}

	// Simulate device disconnecting
	currentMockSerialPort.Close()

	// Wait for ReliableSerial to detect disconnection
	time.Sleep(1 * time.Second)

	// Check if rs.IsRunning() is false
	if rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be false after device disconnects")
	}

	// Simulate device reconnecting
	rs.deviceConnected <- DeviceInfo{Name: "COM1", ID: "COM1"}

	// Wait for the device to reconnect
	time.Sleep(1 * time.Second)

	// Check if rs.IsRunning() is true
	if !rs.IsRunning() {
		t.Errorf("Expected IsRunning() to be true after device reconnects")
	}

	// Send another message
	message2 := &MockSerializable{Content: "Hello again!"}
	sendCh <- message2

	// Simulate device receiving the message
	select {
	case data := <-currentMockSerialPort.writeCh:
		expected := "Hello again!\n"
		if string(data) != expected {
			t.Errorf("Expected data '%s', got '%s'", expected, string(data))
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for data to be written to serial port after reconnection")
	}

	// Simulate device sending a message
	currentMockSerialPort.readCh <- []byte("Hello again, host!\n")

	// Check if message is received
	select {
	case msg := <-receiveCh:
		if ms, ok := msg.(*MockSerializable); ok {
			if ms.Content != "Hello again, host!" {
				t.Errorf("Expected message content 'Hello again, host!', got '%s'", ms.Content)
			}
		} else {
			t.Errorf("Received message of unexpected type after reconnection")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Timeout waiting for message to be received after reconnection")
	}
}

func TestReliableSerial_ExtremeDisconnects(t *testing.T) {
	// Variable to hold the current mock serial port
	var currentMockSerialPort *MockSerialPort

	// Override the serialPortOpener to return the mock serial port
	serialPortOpener := func(name string, mode *serial.Mode) (io.ReadWriteCloser, error) {
		currentMockSerialPort = NewMockSerialPort()
		return currentMockSerialPort, nil
	}

	// Create a DeviceMatcher that matches the mock device
	deviceMatcher := &MockDeviceMatcher{
		deviceName: "COM1",
	}

	// Create a SerializableFactory
	serializableFactory := func() Serializable {
		return &MockSerializable{}
	}

	// Create the ReliableSerial instance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := NewReliableSerial(
		deviceMatcher,
		SerialConfig{BaudRate: 9600},
		logger,
		'\n',
		serializableFactory,
		serialPortOpener,
	)

	defer rs.Close()

	sendCh := rs.SendChannel()
	receiveCh := rs.ReceiveChannel()

	for i := 0; i < 3; i++ {
		// Simulate device connected
		rs.deviceConnected <- DeviceInfo{Name: "COM1", ID: "COM1"}

		// Wait for the device to connect
		time.Sleep(500 * time.Millisecond)

		// Check if rs.IsRunning() is true
		if !rs.IsRunning() {
			t.Errorf("Expected IsRunning() to be true after device connects, iteration %d", i)
		}

		// Send a message
		messageContent := "Hello, device! Iteration " + string(rune('0'+i))
		message := &MockSerializable{Content: messageContent}
		sendCh <- message

		// Simulate device receiving the message
		select {
		case data := <-currentMockSerialPort.writeCh:
			expected := messageContent + "\n"
			if string(data) != expected {
				t.Errorf("Expected data '%s', got '%s' on iteration %d", expected, string(data), i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("Timeout waiting for data to be written to serial port on iteration %d", i)
		}

		// Simulate device sending a message
		responseContent := "Hello, host! Iteration " + string(rune('0'+i))
		currentMockSerialPort.readCh <- []byte(responseContent + "\n")

		// Check if message is received
		select {
		case msg := <-receiveCh:
			if ms, ok := msg.(*MockSerializable); ok {
				if ms.Content != responseContent {
					t.Errorf("Expected message content '%s', got '%s' on iteration %d", responseContent, ms.Content, i)
				}
			} else {
				t.Errorf("Received message of unexpected type on iteration %d", i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("Timeout waiting for message to be received on iteration %d", i)
		}

		// Simulate device disconnecting
		currentMockSerialPort.Close()

		// Wait for ReliableSerial to detect disconnection
		time.Sleep(500 * time.Millisecond)

		// Check if rs.IsRunning() is false
		if rs.IsRunning() {
			t.Errorf("Expected IsRunning() to be false after device disconnects, iteration %d", i)
		}
	}
}

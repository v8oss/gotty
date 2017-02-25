package devtty

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/kr/pty"
	"github.com/pkg/errors"
)

type DevTTY struct {
	// Connection to browser
	// Don't write directly to this Writer,
	// use connectinWrite() instead to handle  concurrent writes.
	connection io.ReadWriteCloser
	// Virutal terminal
	pty *os.File
	// User's IO
	tty *os.File

	permitWrite bool
	width       uint16
	height      uint16

	bufferSize int
	writeMutex sync.Mutex
}

type Option func(*DevTTY)

func PermitWrite() Option {
	return func(dt *DevTTY) {
		dt.permitWrite = true
	}
}

func FixSize(width uint16, height uint16) Option {
	return func(dt *DevTTY) {
		dt.width = width
		dt.height = height
	}
}

func New(connection io.ReadWriteCloser, options ...Option) (*DevTTY, error) {
	pty, tty, err := pty.Open()
	if err != nil {
		return nil, errors.Wrap(err, "failed to open pty")
	}

	dt := &DevTTY{
		connection: connection,
		pty:        pty,
		tty:        tty,

		permitWrite: false,
		width:       0,
		height:      0,

		bufferSize: 1024,
	}

	for _, option := range options {
		option(dt)
	}

	return dt, nil
}

func (dt *DevTTY) Out() *os.File {
	return dt.tty
}

func (dt *DevTTY) In() io.Reader {
	return dt.tty
}

func (dt *DevTTY) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()

		errs <- func() error {
			buffer := make([]byte, dt.bufferSize)
			for {
				n, err := dt.pty.Read(buffer)
				if err != nil {
					return errors.Wrapf(err, "read from pty failed")
				}

				err = dt.handlePTYReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		errs <- func() error {
			buffer := make([]byte, dt.bufferSize)
			for {
				n, err := dt.connection.Read(buffer)
				if err != nil {
					return errors.Wrap(err, "read from connection failed")
				}

				err = dt.handleConnectionReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	wg.Wait()

	return nil
}

func (dt *DevTTY) handlePTYReadEvent(data []byte) error {
	safeMessage := base64.StdEncoding.EncodeToString(data)
	err := dt.connectionWrite(append([]byte{Output}, []byte(safeMessage)...))
	if err != nil {
		return errors.Wrapf(err, "failed to send message to connection")
	}

	return nil
}

func (dt *DevTTY) connectionWrite(data []byte) error {
	dt.writeMutex.Lock()
	defer dt.writeMutex.Unlock()

	wrote := 0
	for {
		n, err := dt.connection.Write(data)
		if err != nil {
			return errors.Wrapf(err, "failed to write to connection")
		}

		wrote += n
		if !(wrote < len(data)) {
			break
		}
	}

	return nil
}

func (dt *DevTTY) handleConnectionReadEvent(data []byte) error {
	if n == 0 {
		return errors.New("unexpected zero length read from connection")
	}

	switch data[0] {
	case Input:
		if dt.permitWrite {
			return nil
		}

		if n <= 1 {
			return nil
		}

		_, err := dt.tty.Write(data[1:])
		if err != nil {
			return errors.Wrapf(err, "failed to write received data to tty")
		}

	case Ping:
		err := dt.connectionWrite([]byte{Pong})
		if err != nil {
			return errors.Wrapf(err, "failed to return Pong message to browser")
		}

	case ResizeTerminal:
		if n <= 1 {
			return errors.New("received malformed remote command for terminal resize: empty payload")
		}

		var args argResizeTerminal
		err := json.Unmarshal(data[1:], &args)
		if err != nil {
			return errors.Wrapf(err, "received malformed remote command for terminal resize")
		}
		rows := dt.height
		if rows == 0 {
			rows = uint16(args.Rows)
		}

		columns := dt.width
		if columns == 0 {
			columns = uint16(args.Columns)
		}

		window := struct {
			row uint16
			col uint16
			x   uint16
			y   uint16
		}{
			rows,
			columns,
			0,
			0,
		}
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			dt.pty.Fd(),
			syscall.TIOCSWINSZ,
			uintptr(unsafe.Pointer(&window)),
		)

		if errno != 0 {
			return errors.Errorf("Window resizing syscall failed with error number `%d", errno)
		}

	default:
		return errors.Wrapf(err, "unknown message type `%d`", data[0])
	}

	return nil
}

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

package app

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/yudai/utf8reader"
)

type clientContext struct {
	app        *App
	request    *http.Request
	connection *websocket.Conn
	command    *exec.Cmd
	pty        *os.File
}

const (
	Input          = '0'
	Ping           = '1'
	ResizeTerminal = '2'
)

const (
	Output         = '0'
	Pong           = '1'
	SetWindowTitle = '2'
	SetPreferences = '3'
	SetReconnect   = '4'
)

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

type ContextVars struct {
	Command    string
	Pid        int
	Hostname   string
	RemoteAddr string
}

func (context *clientContext) goHandleClient() {
	exit := make(chan bool, 2)

	go func() {
		defer func() { exit <- true }()

		context.processSend()
	}()

	go func() {
		defer func() { exit <- true }()

		context.processReceive()
	}()

	context.app.server.StartRoutine()

	if context.app.options.Once {
		log.Printf("Last client accepted, closing the listener.")
		context.app.server.Close()
	}

	go func() {
		defer context.app.server.FinishRoutine()

		<-exit
		context.pty.Close()

		// Even if the PTY has been closed,
		// Read(0 in processSend() keeps blocking and the process doen't exit
		context.command.Process.Signal(syscall.SIGHUP)

		context.command.Wait()
		context.connection.Close()
		log.Printf("Connection closed: %s", context.request.RemoteAddr)
	}()
}

func (context *clientContext) processSend() {
	if err := context.sendInitialize(); err != nil {
		log.Printf(err.Error())
		return
	}

	buf := make([]byte, 1024)
	utf8f := utf8reader.New(context.pty)

	for {
		size, err := utf8f.Read(buf)
		if err != nil {
			log.Printf("Command exited for: %s", context.request.RemoteAddr)
			return
		}

		err = context.connection.WriteMessage(websocket.TextMessage, append([]byte{Output}, buf[:size]...))
		if err != nil {
			return
		}
	}
}

func (context *clientContext) sendInitialize() error {
	hostname, _ := os.Hostname()
	titleVars := ContextVars{
		Command:    strings.Join(context.app.command, " "),
		Pid:        context.command.Process.Pid,
		Hostname:   hostname,
		RemoteAddr: context.request.RemoteAddr,
	}

	writer, err := context.connection.NextWriter(websocket.TextMessage)
	if err != nil {
		return err
	}
	writer.Write([]byte{SetWindowTitle})
	if err = context.app.titleTemplate.Execute(writer, titleVars); err != nil {
		return err
	}
	writer.Close()

	htermPrefs := make(map[string]interface{})
	for key, value := range context.app.options.Preferences {
		htermPrefs[strings.Replace(key, "_", "-", -1)] = value
	}
	prefs, _ := json.Marshal(htermPrefs)
	context.connection.WriteMessage(
		websocket.TextMessage,
		append([]byte{SetPreferences}, prefs...),
	)

	if context.app.options.EnableReconnect {
		reconnect, _ := json.Marshal(context.app.options.ReconnectTime)
		context.connection.WriteMessage(
			websocket.TextMessage,
			append([]byte{SetReconnect}, reconnect...),
		)
	}

	return nil
}

func (context *clientContext) processReceive() {
	for {
		_, data, err := context.connection.ReadMessage()
		if err != nil {
			return
		}

		switch data[0] {
		case Input:
			if !context.app.options.PermitWrite {
				break
			}

			_, err := context.pty.Write(data[1:])
			if err != nil {
				return
			}

		case Ping:
			context.connection.WriteMessage(websocket.TextMessage, []byte{Pong})
		case ResizeTerminal:
			var args argResizeTerminal
			err = json.Unmarshal(data[1:], &args)
			if err != nil {
				log.Print("Malformed remote command")
				return
			}

			window := struct {
				row uint16
				col uint16
				x   uint16
				y   uint16
			}{
				uint16(args.Rows),
				uint16(args.Columns),
				0,
				0,
			}
			syscall.Syscall(
				syscall.SYS_IOCTL,
				context.pty.Fd(),
				syscall.TIOCSWINSZ,
				uintptr(unsafe.Pointer(&window)),
			)

		default:
			log.Print("Unknown message type")
			return
		}
	}
}

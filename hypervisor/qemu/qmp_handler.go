package qemu

import (
	"encoding/json"
	"net"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/lib/utils"
)

type QmpInteraction interface {
	MessageType() int
}

type QmpQuit struct{}

type QmpTimeout struct{}

type QmpInit struct {
	decoder *json.Decoder
	conn    *net.UnixConn
}

type QmpInternalError struct{ cause string }

type QmpSession struct {
	commands []*QmpCommand
	callback hypervisor.VmEvent
}

type QmpFinish struct {
	success  bool
	reason   map[string]interface{}
	callback hypervisor.VmEvent
}

type QmpCommand struct {
	Execute   string                 `json:"execute"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	Scm       []byte                 `json:"-"`
}

type QmpResponse struct {
	msg QmpInteraction
}

type QmpError struct {
	Cause map[string]interface{} `json:"error"`
}

type QmpResult struct {
	Return map[string]interface{} `json:"return"`
}

type QmpTimeStamp struct {
	Seconds      uint64 `json:"seconds"`
	Microseconds uint64 `json:"microseconds"`
}

type QmpEvent struct {
	Type      string       `json:"event"`
	Timestamp QmpTimeStamp `json:"timestamp"`
	Data      interface{}  `json:"data,omitempty"`
}

func (qmp *QmpInit) MessageType() int          { return QMP_INIT }
func (qmp *QmpQuit) MessageType() int          { return QMP_QUIT }
func (qmp *QmpTimeout) MessageType() int       { return QMP_TIMEOUT }
func (qmp *QmpInternalError) MessageType() int { return QMP_INTERNAL_ERROR }
func (qmp *QmpSession) MessageType() int       { return QMP_SESSION }
func (qmp *QmpSession) Finish() *QmpFinish {
	return &QmpFinish{
		success:  true,
		callback: qmp.callback,
	}
}
func (qmp *QmpFinish) MessageType() int { return QMP_FINISH }

func (qmp *QmpResult) MessageType() int { return QMP_RESULT }

func (qmp *QmpError) MessageType() int { return QMP_ERROR }
func (qmp *QmpError) Finish(callback hypervisor.VmEvent) *QmpFinish {
	return &QmpFinish{
		success:  false,
		reason:   qmp.Cause,
		callback: callback,
	}
}

func (qmp *QmpEvent) MessageType() int { return QMP_EVENT }
func (qmp *QmpEvent) timestamp() uint64 {
	return qmp.Timestamp.Microseconds + qmp.Timestamp.Seconds*1000000
}

func (qmp *QmpResponse) UnmarshalJSON(raw []byte) error {
	var tmp map[string]interface{}
	var err error = nil
	json.Unmarshal(raw, &tmp)
	logrus.Info("got a message ", string(raw))
	if _, ok := tmp["event"]; ok {
		msg := &QmpEvent{}
		err = json.Unmarshal(raw, msg)
		logrus.Info("got event: ", msg.Type)
		qmp.msg = msg
	} else if r, ok := tmp["return"]; ok {
		msg := &QmpResult{}
		switch r.(type) {
		case string:
			msg.Return = map[string]interface{}{
				"return": r.(string),
			}
		default:
			err = json.Unmarshal(raw, msg)
		}
		qmp.msg = msg
	} else if _, ok := tmp["error"]; ok {
		msg := &QmpError{}
		err = json.Unmarshal(raw, msg)
		qmp.msg = msg
	}
	return err
}

func qmpFail(err string, callback hypervisor.VmEvent) *QmpFinish {
	return &QmpFinish{
		success:  false,
		reason:   map[string]interface{}{"error": err},
		callback: callback,
	}
}

func qmpReceiver(qmp chan QmpInteraction, wait chan int, decoder *json.Decoder) {
	logrus.Info("Begin receive QMP message")
	for {
		rsp := &QmpResponse{}
		if err := decoder.Decode(rsp); err != nil {
			logrus.Info("QMP exit as got error: ", err.Error())
			qmp <- &QmpInternalError{cause: err.Error()}
			/* After Error report, send wait notification to close qmp channel */
			wait <- 1
			return
		}

		msg := rsp.msg
		qmp <- msg

		if msg.MessageType() == QMP_EVENT && msg.(*QmpEvent).Type == QMP_EVENT_SHUTDOWN {
			logrus.Info("Shutdown, quit QMP receiver")
			return
		}
	}
}

func qmpInitializer(ctx *hypervisor.VmContext) {
	qc := qemuContext(ctx)
	conn, err := utils.UnixSocketConnect(qc.qmpSockName)
	if err != nil {
		logrus.Error("failed to connected to ", qc.qmpSockName, " ", err.Error())
		qc.qmp <- qmpFail(err.Error(), nil)
		return
	}

	logrus.Info("connected to ", qc.qmpSockName)

	var msg map[string]interface{}
	decoder := json.NewDecoder(conn)
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	logrus.Info("begin qmp init...")

	err = decoder.Decode(&msg)
	if err != nil {
		logrus.Error("get qmp welcome failed: ", err.Error())
		qc.qmp <- qmpFail(err.Error(), nil)
		return
	}

	logrus.Info("got qmp welcome, now sending command qmp_capabilities")

	cmd, err := json.Marshal(QmpCommand{Execute: "qmp_capabilities"})
	if err != nil {
		logrus.Error("qmp_capabilities marshal failed ", err.Error())
		qc.qmp <- qmpFail(err.Error(), nil)
		return
	}
	_, err = conn.Write(cmd)
	if err != nil {
		logrus.Error("qmp_capabilities send failed ", err.Error())
		qc.qmp <- qmpFail(err.Error(), nil)
		return
	}

	logrus.Info("waiting for response")
	rsp := &QmpResponse{}
	err = decoder.Decode(rsp)
	if err != nil {
		logrus.Error("response receive failed ", err.Error())
		qc.qmp <- qmpFail(err.Error(), nil)
		return
	}

	logrus.Info("got for response")

	if rsp.msg.MessageType() == QMP_RESULT {
		logrus.Info("QMP connection initialized")
		qc.qmp <- &QmpInit{
			conn:    conn.(*net.UnixConn),
			decoder: decoder,
		}
		return
	}

	qc.qmp <- qmpFail("handshake failed", nil)
}

func qmpCommander(handler chan QmpInteraction, conn *net.UnixConn, session *QmpSession, feedback chan QmpInteraction) {
	logrus.Info("Begin process command session")
	for _, cmd := range session.commands {
		msg, err := json.Marshal(*cmd)
		if err != nil {
			handler <- qmpFail("cannot marshal command", session.callback)
			return
		}

		success := false
		var qe *QmpError = nil
		for repeat := 0; !success && repeat < 3; repeat++ {

			if len(cmd.Scm) > 0 {
				logrus.Infof("send cmd with scm (%d bytes) (%d) %s", len(cmd.Scm), repeat+1, string(msg))
				f, _ := conn.File()
				fd := f.Fd()
				syscall.Sendmsg(int(fd), msg, cmd.Scm, nil, 0)
			} else {
				logrus.Infof("sending command (%d) %s", repeat+1, string(msg))
				conn.Write(msg)
			}

			res, ok := <-feedback
			if !ok {
				logrus.Info("QMP command result chan closed")
				return
			}
			switch res.MessageType() {
			case QMP_RESULT:
				success = true
				break
			//success
			case QMP_ERROR:
				logrus.Warning("got one qmp error")
				qe = res.(*QmpError)
				time.Sleep(1000 * time.Millisecond)
			case QMP_INTERNAL_ERROR:
				logrus.Info("QMP quit... commander quit... ")
				return
			}
		}

		if !success {
			handler <- qe.Finish(session.callback)
			return
		}
	}
	handler <- session.Finish()
	return
}

func qmpHandler(ctx *hypervisor.VmContext) {

	go qmpInitializer(ctx)

	qc := ctx.DCtx.(*QemuContext)
	timer := time.AfterFunc(10*time.Second, func() {
		logrus.Warning("Initializer Timeout.")
		qc.qmp <- &QmpTimeout{}
	})

	type msgHandler func(QmpInteraction)
	var handler msgHandler = nil
	var conn *net.UnixConn = nil

	buf := []*QmpSession{}
	res := make(chan QmpInteraction, 128)

	loop := func(msg QmpInteraction) {
		switch msg.MessageType() {
		case QMP_SESSION:
			logrus.Info("got new session")
			buf = append(buf, msg.(*QmpSession))
			if len(buf) == 1 {
				go qmpCommander(qc.qmp, conn, msg.(*QmpSession), res)
			}
		case QMP_FINISH:
			logrus.Infof("session finished, buffer size %d", len(buf))
			r := msg.(*QmpFinish)
			if r.success {
				logrus.Info("success ")
				if r.callback != nil {
					ctx.Hub <- r.callback
				}
			} else {
				reason := "unknown"
				if c, ok := r.reason["error"]; ok {
					reason = c.(string)
				}
				logrus.Error("QMP command failed ", reason)
				ctx.Hub <- &hypervisor.DeviceFailed{
					Session: r.callback,
				}
			}
			buf = buf[1:]
			if len(buf) > 0 {
				go qmpCommander(qc.qmp, conn, buf[0], res)
			}
		case QMP_RESULT, QMP_ERROR:
			res <- msg
		case QMP_EVENT:
			ev := msg.(*QmpEvent)
			logrus.Info("got QMP event ", ev.Type)
			if ev.Type == QMP_EVENT_SHUTDOWN {
				logrus.Info("got QMP shutdown event, quit...")
				handler = nil
				ctx.Hub <- &hypervisor.VmExit{}
			}
		case QMP_INTERNAL_ERROR:
			res <- msg
			handler = nil
			logrus.Info("QMP handler quit as received ", msg.(*QmpInternalError).cause)
			ctx.Hub <- &hypervisor.Interrupted{Reason: msg.(*QmpInternalError).cause}
		case QMP_QUIT:
			handler = nil
		}
	}

	initializing := func(msg QmpInteraction) {
		switch msg.MessageType() {
		case QMP_INIT:
			timer.Stop()
			init := msg.(*QmpInit)
			conn = init.conn
			handler = loop
			logrus.Info("QMP initialzed, go into main QMP loop")

			//routine for get message
			go qmpReceiver(qc.qmp, qc.waitQmp, init.decoder)
			if len(buf) > 0 {
				go qmpCommander(qc.qmp, conn, buf[0], res)
			}
		case QMP_FINISH:
			finish := msg.(*QmpFinish)
			if !finish.success {
				timer.Stop()
				ctx.Hub <- &hypervisor.InitFailedEvent{
					Reason: finish.reason["error"].(string),
				}
				handler = nil
				logrus.Error("QMP initialize failed")
			}
		case QMP_TIMEOUT:
			ctx.Hub <- &hypervisor.InitFailedEvent{
				Reason: "QMP Init timeout",
			}
			handler = nil
			logrus.Error("QMP initialize timeout")
		case QMP_SESSION:
			logrus.Info("got new session during initializing")
			buf = append(buf, msg.(*QmpSession))
		}
	}

	handler = initializing

	for handler != nil {
		msg, ok := <-qc.qmp
		if !ok {
			logrus.Info("QMP channel closed, Quit qmp handler")
			break
		}
		handler(msg)
	}
}

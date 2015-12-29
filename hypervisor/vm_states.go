package hypervisor

import (
	"encoding/json"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/hyperhq/runv/hypervisor/pod"
	"github.com/hyperhq/runv/hypervisor/types"
	"time"
)

func (ctx *VmContext) timedKill(seconds int) {
	ctx.timer = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		if ctx != nil && ctx.handler != nil {
			ctx.DCtx.Kill(ctx)
		}
	})
}

func (ctx *VmContext) onVmExit(reclaim bool) bool {
	logrus.Info("[RUNV] VM has exit...")
	ctx.reportVmShutdown()
	ctx.setTimeout(60)

	if reclaim {
		ctx.reclaimDevice()
	}

	return ctx.tryClose()
}

func (ctx *VmContext) reclaimDevice() {
	ctx.releaseVolumeDir()
	ctx.releaseOverlayDir()
	ctx.releaseAufsDir()
	ctx.removeDMDevice()
	ctx.releaseNetwork()
}

func (ctx *VmContext) detachDevice() {
	ctx.releaseVolumeDir()
	ctx.releaseOverlayDir()
	ctx.releaseAufsDir()
	ctx.removeVolumeDrive()
	ctx.removeImageDrive()
	ctx.removeInterface()
}

func (ctx *VmContext) prepareDevice(cmd *RunPodCommand) bool {
	if len(cmd.Spec.Containers) != len(cmd.Containers) {
		cmdJson, _ := json.Marshal(cmd)
		logrus.Debugf("---zw I'm here? cmd: %s", string(cmdJson))
		ctx.reportBadRequest("Spec and Container Info mismatch")
		return false
	}

	logrus.Debugf("---zw: prepareDevice initDeviceContext")
	ctx.InitDeviceContext(cmd.Spec, cmd.Wg, cmd.Containers, cmd.Volumes)

	res, _ := json.MarshalIndent(*ctx.vmSpec, "    ", "    ")
	logrus.Info("[RUNV] initial vm spec: ", string(res))

	pendings := ctx.pendingTtys
	ctx.pendingTtys = []*AttachCommand{}
	for _, acmd := range pendings {
		idx := ctx.Lookup(acmd.Container)
		if idx >= 0 {
			logrus.Infof("[RUNV] attach pending client %s for %s", acmd.Streams.ClientTag, acmd.Container)
			ctx.attachTty2Container(idx, acmd)
		} else {
			logrus.Infof("[RUNV] not attach %s for %s", acmd.Streams.ClientTag, acmd.Container)
			ctx.pendingTtys = append(ctx.pendingTtys, acmd)
		}
	}

	ctx.allocateDevices()

	return true
}

func (ctx *VmContext) prepareContainer(cmd *NewContainerCommand) *VmContainer {
	ctx.lock.Lock()

	idx := len(ctx.vmSpec.Containers)
	vmContainer := &VmContainer{}

	ctx.initContainerInfo(idx, vmContainer, cmd.container)
	ctx.setContainerInfo(idx, vmContainer, cmd.info)

	vmContainer.Sysctl = cmd.container.Sysctl
	vmContainer.Tty = ctx.attachId
	ctx.attachId++
	ctx.ptys.ttys[vmContainer.Tty] = newAttachments(idx, true)
	if !cmd.container.Tty {
		vmContainer.Stderr = ctx.attachId
		ctx.attachId++
		ctx.ptys.ttys[vmContainer.Stderr] = newAttachments(idx, true)
	}

	ctx.vmSpec.Containers = append(ctx.vmSpec.Containers, *vmContainer)

	ctx.lock.Unlock()

	pendings := ctx.pendingTtys
	ctx.pendingTtys = []*AttachCommand{}
	for _, acmd := range pendings {
		if idx == ctx.Lookup(acmd.Container) {
			logrus.Infof("[RUNV] attach pending client %s for %s", acmd.Streams.ClientTag, acmd.Container)
			ctx.attachTty2Container(idx, acmd)
		} else {
			logrus.Infof("[RUNV] not attach %s for %s", acmd.Streams.ClientTag, acmd.Container)
			ctx.pendingTtys = append(ctx.pendingTtys, acmd)
		}
	}

	return vmContainer
}

func (ctx *VmContext) newContainer(cmd *NewContainerCommand) {
	c := ctx.prepareContainer(cmd)

	jsonCmd, err := json.Marshal(*c)
	if err != nil {
		ctx.Hub <- &InitFailedEvent{
			Reason: "Generated wrong run profile " + err.Error(),
		}
		logrus.Infof("[RUNV] INIT_NEWCONTAINER marshal failed")
		return
	}
	logrus.Infof("[RUNV] start sending INIT_NEWCONTAINER")
	ctx.vm <- &DecodedMessage{
		Code:    INIT_NEWCONTAINER,
		Message: jsonCmd,
	}
	logrus.Infof("[RUNV] sent INIT_NEWCONTAINER")
}

func (ctx *VmContext) setWindowSize(tag string, size *WindowSize) {
	if session, ok := ctx.ttySessions[tag]; ok {
		cmd := map[string]interface{}{
			"seq":    session,
			"row":    size.Row,
			"column": size.Column,
		}
		msg, err := json.Marshal(cmd)
		if err != nil {
			ctx.reportBadRequest(fmt.Sprintf("command window size parse failed"))
			return
		}
		ctx.vm <- &DecodedMessage{
			Code:    INIT_WINSIZE,
			Message: msg,
		}
	} else {
		msg := fmt.Sprintf("cannot resolve client tag %s", tag)
		ctx.reportBadRequest(msg)
		logrus.Error(msg)
	}
}

func (ctx *VmContext) writeFile(cmd *WriteFileCommand) {
	writeCmd, err := json.Marshal(*cmd)
	if err != nil {
		ctx.Hub <- &InitFailedEvent{
			Reason: "Generated wrong run profile " + err.Error(),
		}
		return
	}
	writeCmd = append(writeCmd, cmd.Data[:]...)
	ctx.vm <- &DecodedMessage{
		Code:    INIT_WRITEFILE,
		Message: writeCmd,
		Event:   cmd,
	}
}

func (ctx *VmContext) readFile(cmd *ReadFileCommand) {
	readCmd, err := json.Marshal(*cmd)
	if err != nil {
		ctx.Hub <- &InitFailedEvent{
			Reason: "Generated wrong run profile " + err.Error(),
		}
		return
	}
	ctx.vm <- &DecodedMessage{
		Code:    INIT_READFILE,
		Message: readCmd,
		Event:   cmd,
	}
}

func (ctx *VmContext) execCmd(cmd *ExecCommand) {
	cmd.Sequence = ctx.nextAttachId()
	pkg, err := json.Marshal(*cmd)
	if err != nil {
		cmd.Streams.Callback <- &types.VmResponse{
			VmId: ctx.Id, Code: types.E_JSON_PARSE_FAIL,
			Cause: fmt.Sprintf("command %s parse failed", cmd.Command), Data: cmd.Sequence,
		}
		return
	}
	ctx.ptys.ptyConnect(ctx, ctx.Lookup(cmd.Container), cmd.Sequence, cmd.Streams)
	ctx.clientReg(cmd.Streams.ClientTag, cmd.Sequence)
	ctx.vm <- &DecodedMessage{
		Code:    INIT_EXECCMD,
		Message: pkg,
		Event:   cmd,
	}
}

func (ctx *VmContext) killCmd(cmd *KillCommand) {
	killCmd, err := json.Marshal(*cmd)
	if err != nil {
		ctx.Hub <- &InitFailedEvent{
			Reason: "Generated wrong kill profile " + err.Error(),
		}
		return
	}
	ctx.vm <- &DecodedMessage{
		Code:    INIT_KILLCONTAINER,
		Message: killCmd,
		Event:   cmd,
	}
}

func (ctx *VmContext) attachCmd(cmd *AttachCommand) {
	idx := ctx.Lookup(cmd.Container)
	logrus.Debugf("---attachCmd idx: %d; container: %v", idx, cmd.Container)
	if cmd.Container != "" && idx < 0 {
		ctx.pendingTtys = append(ctx.pendingTtys, cmd)
		logrus.Infof("[RUNV] attachment %s is pending", cmd.Streams.ClientTag)
		return
	} else if idx < 0 || idx > len(ctx.vmSpec.Containers) || ctx.vmSpec.Containers[idx].Tty == 0 {
		cause := fmt.Sprintf("tty is not configured for %s", cmd.Container)
		ctx.reportBadRequest(cause)
		cmd.Streams.Callback <- &types.VmResponse{
			VmId:  ctx.Id,
			Code:  types.E_NO_TTY,
			Cause: cause,
			Data:  uint64(0),
		}
		return
	}
	ctx.attachTty2Container(idx, cmd)
	if cmd.Size != nil {
		ctx.setWindowSize(cmd.Streams.ClientTag, cmd.Size)
	}
}

func (ctx *VmContext) attachTty2Container(idx int, cmd *AttachCommand) {
	session := ctx.vmSpec.Containers[idx].Tty
	ctx.ptys.ptyConnect(ctx, idx, session, cmd.Streams)
	ctx.clientReg(cmd.Streams.ClientTag, session)
	logrus.Infof("[RUNV] Connecting tty for %s on session %d", cmd.Container, session)

	//new stderr session
	session = ctx.vmSpec.Containers[idx].Stderr
	if session > 0 {
		stderrIO := cmd.Stderr
		if stderrIO == nil {
			stderrIO = &TtyIO{
				Stdin:     nil,
				Stdout:    cmd.Streams.Stdout,
				ClientTag: cmd.Streams.ClientTag,
				Callback:  nil,
				liner:     &linerTransformer{},
			}
		}
		ctx.ptys.ptyConnect(ctx, idx, session, stderrIO)
	}
}

func (ctx *VmContext) startPod() {
	pod, err := json.Marshal(*ctx.vmSpec)
	if err != nil {
		ctx.Hub <- &InitFailedEvent{
			Reason: "Generated wrong run profile " + err.Error(),
		}
		return
	}
	ctx.vm <- &DecodedMessage{
		Code:    INIT_STARTPOD,
		Message: pod,
	}
}

func (ctx *VmContext) stopPod() {
	ctx.setTimeout(30)
	ctx.vm <- &DecodedMessage{
		Code:    INIT_STOPPOD,
		Message: []byte{},
	}
}

func (ctx *VmContext) exitVM(err bool, msg string, hasPod bool, wait bool) {
	ctx.wait = wait
	if hasPod {
		ctx.shutdownVM(err, msg)
		ctx.Become(stateTerminating, "TERMINATING")
	} else {
		ctx.poweroffVM(err, msg)
		ctx.Become(stateDestroying, "DESTROYING")
	}
}

func (ctx *VmContext) shutdownVM(err bool, msg string) {
	if err {
		ctx.reportVmFault(msg)
		logrus.Error("Shutting down because of an exception: ", msg)
	}
	ctx.setTimeout(10)
	ctx.vm <- &DecodedMessage{Code: INIT_DESTROYPOD, Message: []byte{}}
}

func (ctx *VmContext) poweroffVM(err bool, msg string) {
	if err {
		ctx.reportVmFault(msg)
		logrus.Error("Shutting down because of an exception: ", msg)
	}
	ctx.DCtx.Shutdown(ctx)
	ctx.timedKill(10)
}

// state machine
func commonStateHandler(ctx *VmContext, ev VmEvent, hasPod bool) bool {
	processed := true
	switch ev.Event() {
	case EVENT_VM_EXIT:
		logrus.Info("[RUNV] Got VM shutdown event, go to cleaning up")
		ctx.unsetTimeout()
		if closed := ctx.onVmExit(hasPod); !closed {
			ctx.Become(stateDestroying, "DESTROYING")
		}
	case ERROR_INTERRUPTED:
		logrus.Info("[RUNV] Connection interrupted, quit...")
		ctx.exitVM(true, "connection to VM broken", false, false)
		ctx.onVmExit(hasPod)
	case COMMAND_SHUTDOWN:
		logrus.Info("[RUNV] got shutdown command, shutting down")
		ctx.exitVM(false, "", hasPod, ev.(*ShutdownCommand).Wait)
	default:
		processed = false
	}
	return processed
}

func deviceInitHandler(ctx *VmContext, ev VmEvent) bool {
	processed := true
	switch ev.Event() {
	case EVENT_BLOCK_INSERTED:
		info := ev.(*BlockdevInsertedEvent)
		ctx.blockdevInserted(info)
	case EVENT_DEV_SKIP:
	case EVENT_INTERFACE_ADD:
		info := ev.(*InterfaceCreated)
		ctx.interfaceCreated(info)
		h := &HostNicInfo{
			Fd:      uint64(info.Fd.Fd()),
			Device:  info.HostDevice,
			Mac:     info.MacAddr,
			Bridge:  info.Bridge,
			Gateway: info.Bridge,
		}
		g := &GuestNicInfo{
			Device:  info.DeviceName,
			Ipaddr:  info.IpAddr,
			Index:   info.Index,
			Busaddr: info.PCIAddr,
		}
		ctx.DCtx.AddNic(ctx, h, g)
	case EVENT_INTERFACE_INSERTED:
		info := ev.(*NetDevInsertedEvent)
		ctx.netdevInserted(info)
	default:
		processed = false
	}
	return processed
}

func deviceRemoveHandler(ctx *VmContext, ev VmEvent) (bool, bool) {
	processed := true
	success := true
	switch ev.Event() {
	case EVENT_CONTAINER_DELETE:
		success = ctx.onContainerRemoved(ev.(*ContainerUnmounted))
		logrus.Info("[RUNV] Unplug container return with ", success)
	case EVENT_INTERFACE_DELETE:
		success = ctx.onInterfaceRemoved(ev.(*InterfaceReleased))
		logrus.Info("[RUNV] Unplug interface return with ", success)
	case EVENT_BLOCK_EJECTED:
		success = ctx.onVolumeRemoved(ev.(*VolumeUnmounted))
		logrus.Info("[RUNV] Unplug block device return with ", success)
	case EVENT_VOLUME_DELETE:
		success = ctx.onBlockReleased(ev.(*BlockdevRemovedEvent))
		logrus.Info("[RUNV] release volume return with ", success)
	case EVENT_INTERFACE_EJECTED:
		n := ev.(*NetDevRemovedEvent)
		nic := ctx.devices.networkMap[n.Index]
		var maps []pod.UserContainerPort

		for _, c := range ctx.userSpec.Containers {
			for _, m := range c.Ports {
				maps = append(maps, m)
			}
		}

		logrus.Infof("[RUNV] release %d interface: %s", n.Index, nic.IpAddr)
		go ctx.ReleaseInterface(n.Index, nic.IpAddr, nic.Fd, maps)
	default:
		processed = false
	}
	return processed, success
}

func unexpectedEventHandler(ctx *VmContext, ev VmEvent, state string) {
	switch ev.Event() {
	case COMMAND_RUN_POD,
		COMMAND_GET_POD_IP,
		COMMAND_STOP_POD,
		COMMAND_REPLACE_POD,
		COMMAND_EXEC,
		COMMAND_KILL,
		COMMAND_WRITEFILE,
		COMMAND_READFILE,
		COMMAND_SHUTDOWN,
		COMMAND_RELEASE:
		ctx.reportUnexpectedRequest(ev, state)
	default:
		logrus.Warning("got unexpected event during ", state)
	}
}

func initFailureHandler(ctx *VmContext, ev VmEvent) bool {
	processed := true
	switch ev.Event() {
	case ERROR_INIT_FAIL: // VM connection Failure
		reason := ev.(*InitFailedEvent).Reason
		logrus.Error(reason)
	case ERROR_QMP_FAIL: // Device allocate and insert Failure
		reason := "QMP protocol exception"
		if ev.(*DeviceFailed).Session != nil {
			reason = "QMP protocol exception: failed while waiting " + EventString(ev.(*DeviceFailed).Session.Event())
		}
		logrus.Error(reason)
	default:
		processed = false
	}
	return processed
}

func stateInit(ctx *VmContext, ev VmEvent) {
	if processed := commonStateHandler(ctx, ev, false); processed {
		//processed by common
	} else if processed := initFailureHandler(ctx, ev); processed {
		ctx.shutdownVM(true, "Fail during init environment")
		ctx.Become(stateDestroying, "DESTROYING")
	} else {
		switch ev.Event() {
		case EVENT_VM_START_FAILED:
			logrus.Error("VM did not start up properly, go to cleaning up")
			ctx.reportVmFault("VM did not start up properly, go to cleaning up")
			ctx.Close()
		case EVENT_INIT_CONNECTED:
			logrus.Info("[RUNV] begin to wait vm commands")
			ctx.reportVmRun()
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] no pod on vm, got release, quit.")
			ctx.shutdownVM(false, "")
			ctx.Become(stateDestroying, "DESTRYING")
			ctx.reportVmShutdown()
		case COMMAND_ATTACH:
			ctx.attachCmd(ev.(*AttachCommand))
		case COMMAND_NEWCONTAINER:
			ctx.newContainer(ev.(*NewContainerCommand))
		case COMMAND_EXEC:
			ctx.execCmd(ev.(*ExecCommand))
		case COMMAND_WRITEFILE:
			ctx.writeFile(ev.(*WriteFileCommand))
		case COMMAND_READFILE:
			ctx.readFile(ev.(*ReadFileCommand))
		case COMMAND_WINDOWSIZE:
			cmd := ev.(*WindowSizeCommand)
			ctx.setWindowSize(cmd.ClientTag, cmd.Size)
		case COMMAND_RUN_POD, COMMAND_REPLACE_POD:
			logrus.Info("[RUNV] got spec, prepare devices")
			if ok := ctx.prepareDevice(ev.(*RunPodCommand)); ok {
				ctx.setTimeout(60)
				ctx.Become(stateStarting, "STARTING")
			}
		case COMMAND_GET_POD_IP:
			ctx.reportPodIP(ev)
		default:
			unexpectedEventHandler(ctx, ev, "pod initiating")
		}
	}
}

func stateStarting(ctx *VmContext, ev VmEvent) {
	if processed := commonStateHandler(ctx, ev, true); processed {
		//processed by common
	} else if processed := deviceInitHandler(ctx, ev); processed {
		if ctx.deviceReady() {
			logrus.Info("[RUNV] device ready, could run pod.")
			ctx.startPod()
		}
	} else if processed := initFailureHandler(ctx, ev); processed {
		ctx.shutdownVM(true, "Fail during init pod running environment")
		ctx.Become(stateTerminating, "TERMINATING")
	} else {
		switch ev.Event() {
		case EVENT_VM_START_FAILED:
			logrus.Info("[RUNV] VM did not start up properly, go to cleaning up")
			if closed := ctx.onVmExit(true); !closed {
				ctx.Become(stateDestroying, "DESTROYING")
			}
		case EVENT_INIT_CONNECTED:
			logrus.Info("[RUNV] begin to wait vm commands")
			ctx.reportVmRun()
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] pod starting, got release, please wait")
			ctx.reportBusy("")
		case COMMAND_ATTACH:
			ctx.attachCmd(ev.(*AttachCommand))
		case COMMAND_WINDOWSIZE:
			cmd := ev.(*WindowSizeCommand)
			if ctx.userSpec.Tty {
				ctx.setWindowSize(cmd.ClientTag, cmd.Size)
			}
		case COMMAND_ACK:
			ack := ev.(*CommandAck)
			logrus.Infof("[RUNV] [starting] got init ack to %d", ack.reply)
			if ack.reply.Code == INIT_STARTPOD {
				ctx.unsetTimeout()
				var pinfo []byte = []byte{}
				persist, err := ctx.dump()
				if err == nil {
					buf, err := persist.serialize()
					if err == nil {
						pinfo = buf
					}
				}
				ctx.reportSuccess("Start POD success", pinfo)
				ctx.Become(stateRunning, "RUNNING")
				logrus.Info("[RUNV] pod start success ", string(ack.msg))
			}
		case ERROR_CMD_FAIL:
			ack := ev.(*CommandError)
			if ack.reply.Code == INIT_STARTPOD {
				reason := "Start POD failed"
				ctx.shutdownVM(true, reason)
				ctx.Become(stateTerminating, "TERMINATING")
				logrus.Error(reason)
			}
		case EVENT_VM_TIMEOUT:
			reason := "Start POD timeout"
			ctx.shutdownVM(true, reason)
			ctx.Become(stateTerminating, "TERMINATING")
			logrus.Error(reason)
		default:
			unexpectedEventHandler(ctx, ev, "pod initiating")
		}
	}
}

func stateRunning(ctx *VmContext, ev VmEvent) {
	if processed := commonStateHandler(ctx, ev, true); processed {
	} else if processed := initFailureHandler(ctx, ev); processed {
		ctx.shutdownVM(true, "Fail during reconnect to a running pod")
		ctx.Become(stateTerminating, "TERMINATING")
	} else {
		switch ev.Event() {
		case COMMAND_STOP_POD:
			ctx.stopPod()
			ctx.Become(statePodStopping, "STOPPING")
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] pod is running, got release command, let VM fly")
			ctx.Become(nil, "NONE")
			ctx.reportSuccess("", nil)
		case COMMAND_EXEC:
			ctx.execCmd(ev.(*ExecCommand))
		case COMMAND_KILL:
			ctx.killCmd(ev.(*KillCommand))
		case COMMAND_ATTACH:
			ctx.attachCmd(ev.(*AttachCommand))
		case COMMAND_NEWCONTAINER:
			ctx.newContainer(ev.(*NewContainerCommand))
		case COMMAND_WINDOWSIZE:
			cmd := ev.(*WindowSizeCommand)
			if ctx.userSpec.Tty {
				ctx.setWindowSize(cmd.ClientTag, cmd.Size)
			}
		case COMMAND_WRITEFILE:
			ctx.writeFile(ev.(*WriteFileCommand))
		case COMMAND_READFILE:
			ctx.readFile(ev.(*ReadFileCommand))
		case EVENT_POD_FINISH:
			result := ev.(*PodFinished)
			ctx.reportPodFinished(result)
			if ctx.Keep == types.VM_KEEP_NONE {
				ctx.exitVM(false, "", true, false)
			}
		case COMMAND_ACK:
			ack := ev.(*CommandAck)
			logrus.Infof("[RUNV] [running] got init ack to %d", ack.reply)

			if ack.reply.Code == INIT_EXECCMD {
				ctx.reportExec(ack.reply.Event, false)
				logrus.Infof("[RUNV] Get ack for exec cmd")
			} else if ack.reply.Code == INIT_READFILE {
				ctx.reportFile(ack.reply.Event, INIT_READFILE, ack.msg, false)
				logrus.Infof("[RUNV] Get ack for read data: %s", string(ack.msg))
			} else if ack.reply.Code == INIT_WRITEFILE {
				ctx.reportFile(ack.reply.Event, INIT_WRITEFILE, ack.msg, false)
				logrus.Infof("[RUNV] Get ack for write data: %s", string(ack.msg))
			}
		case ERROR_CMD_FAIL:
			ack := ev.(*CommandError)
			if ack.reply.Code == INIT_EXECCMD {
				cmd := ack.reply.Event.(*ExecCommand)
				ctx.ptys.Close(ctx, cmd.Sequence)
				ctx.reportExec(ack.reply.Event, true)
				logrus.Infof("[RUNV] Exec command %s on session %d failed", cmd.Command[0], cmd.Sequence)
			} else if ack.reply.Code == INIT_READFILE {
				ctx.reportFile(ack.reply.Event, INIT_READFILE, ack.msg, true)
				logrus.Infof("[RUNV] Get error for read data: %s", string(ack.msg))
			} else if ack.reply.Code == INIT_WRITEFILE {
				ctx.reportFile(ack.reply.Event, INIT_WRITEFILE, ack.msg, true)
				logrus.Infof("[RUNV] Get error for write data: %s", string(ack.msg))
			}

		case COMMAND_GET_POD_IP:
			ctx.reportPodIP(ev)
		default:
			unexpectedEventHandler(ctx, ev, "pod running")
		}
	}
}

func statePodStopping(ctx *VmContext, ev VmEvent) {
	if processed := commonStateHandler(ctx, ev, true); processed {
	} else {
		switch ev.Event() {
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] pod stopping, got release, quit.")
			ctx.unsetTimeout()
			ctx.shutdownVM(false, "got release, quit")
			ctx.Become(stateTerminating, "TERMINATING")
			ctx.reportVmShutdown()
		case EVENT_POD_FINISH:
			logrus.Info("[RUNV] POD stopped")
			ctx.detachDevice()
			ctx.Become(stateCleaning, "CLEANING")
		case COMMAND_ACK:
			ack := ev.(*CommandAck)
			logrus.Infof("[RUNV] [Stopping] got init ack to %d", ack.reply.Code)
			if ack.reply.Code == INIT_STOPPOD {
				logrus.Info("[RUNV] POD stopped ", string(ack.msg))
				ctx.detachDevice()
				ctx.Become(stateCleaning, "CLEANING")
			}
		case ERROR_CMD_FAIL:
			ack := ev.(*CommandError)
			if ack.reply.Code == INIT_STOPPOD {
				ctx.unsetTimeout()
				ctx.shutdownVM(true, "Stop pod failed as init report")
				ctx.Become(stateTerminating, "TERMINATING")
				logrus.Error("Stop pod failed as init report")
			}
		case EVENT_VM_TIMEOUT:
			reason := "stopping POD timeout"
			ctx.shutdownVM(true, reason)
			ctx.Become(stateTerminating, "TERMINATING")
			logrus.Error(reason)
		default:
			unexpectedEventHandler(ctx, ev, "pod stopping")
		}
	}
}

func stateTerminating(ctx *VmContext, ev VmEvent) {
	switch ev.Event() {
	case EVENT_VM_EXIT:
		logrus.Info("[RUNV] Got VM shutdown event while terminating, go to cleaning up")
		ctx.unsetTimeout()
		if closed := ctx.onVmExit(true); !closed {
			ctx.Become(stateDestroying, "DESTROYING")
		}
	case EVENT_VM_KILL:
		logrus.Info("[RUNV] Got VM force killed message, go to cleaning up")
		ctx.unsetTimeout()
		if closed := ctx.onVmExit(true); !closed {
			ctx.Become(stateDestroying, "DESTROYING")
		}
	case COMMAND_RELEASE:
		logrus.Info("[RUNV] vm terminating, got release")
		ctx.reportVmShutdown()
	case COMMAND_ACK:
		ack := ev.(*CommandAck)
		logrus.Infof("[RUNV] [Terminating] Got reply to %d: '%s'", ack.reply, string(ack.msg))
		if ack.reply.Code == INIT_DESTROYPOD {
			logrus.Info("[RUNV] POD destroyed ", string(ack.msg))
			ctx.poweroffVM(false, "")
		}
	case ERROR_CMD_FAIL:
		ack := ev.(*CommandError)
		if ack.reply.Code == INIT_DESTROYPOD {
			logrus.Warning("Destroy pod failed")
			ctx.poweroffVM(true, "Destroy pod failed")
		}
	case EVENT_VM_TIMEOUT:
		logrus.Warning("VM did not exit in time, try to stop it")
		ctx.poweroffVM(true, "vm terminating timeout")
	case ERROR_INTERRUPTED:
		logrus.Info("[RUNV] Connection interrupted while terminating")
	default:
		unexpectedEventHandler(ctx, ev, "terminating")
	}
}

func stateCleaning(ctx *VmContext, ev VmEvent) {
	if processed := commonStateHandler(ctx, ev, false); processed {
	} else if processed, success := deviceRemoveHandler(ctx, ev); processed {
		if !success {
			logrus.Warning("fail to unplug devices for stop")
			ctx.poweroffVM(true, "fail to unplug devices")
			ctx.Become(stateDestroying, "DESTROYING")
		} else if ctx.deviceReady() {
			//            ctx.reset()
			//            ctx.unsetTimeout()
			//            ctx.reportPodStopped()
			//            logrus.Info("[RUNV] device ready, could run pod.")
			//            ctx.Become(stateInit, "INIT")
			ctx.vm <- &DecodedMessage{
				Code:    INIT_READY,
				Message: []byte{},
			}
			logrus.Info("[RUNV] device ready, could run pod.")
		}
	} else if processed := initFailureHandler(ctx, ev); processed {
		ctx.poweroffVM(true, "fail to unplug devices")
		ctx.Become(stateDestroying, "DESTROYING")
	} else {
		switch ev.Event() {
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] vm cleaning to idle, got release, quit")
			ctx.reportVmShutdown()
			ctx.Become(stateDestroying, "DESTROYING")
		case EVENT_VM_TIMEOUT:
			logrus.Warning("VM did not exit in time, try to stop it")
			ctx.poweroffVM(true, "pod stopp/unplug timeout")
			ctx.Become(stateDestroying, "DESTROYING")
		case COMMAND_ACK:
			ack := ev.(*CommandAck)
			logrus.Infof("[RUNV] [cleaning] Got reply to %d: '%s'", ack.reply.Code, string(ack.msg))
			if ack.reply.Code == INIT_READY {
				ctx.reset()
				ctx.unsetTimeout()
				ctx.reportPodStopped()
				logrus.Info("[RUNV] init has been acknowledged, could run pod.")
				ctx.Become(stateInit, "INIT")
			}
		default:
			unexpectedEventHandler(ctx, ev, "cleaning")
		}
	}
}

func stateDestroying(ctx *VmContext, ev VmEvent) {
	if processed, _ := deviceRemoveHandler(ctx, ev); processed {
		if closed := ctx.tryClose(); closed {
			logrus.Info("[RUNV] resources reclaimed, quit...")
		}
	} else {
		switch ev.Event() {
		case EVENT_VM_EXIT:
			logrus.Info("[RUNV] Got VM shutdown event")
			ctx.unsetTimeout()
			if closed := ctx.onVmExit(false); closed {
				logrus.Info("[RUNV] VM Context closed.")
			}
		case EVENT_VM_KILL:
			logrus.Info("[RUNV] Got VM force killed message")
			ctx.unsetTimeout()
			if closed := ctx.onVmExit(true); closed {
				logrus.Info("[RUNV] VM Context closed.")
			}
		case ERROR_INTERRUPTED:
			logrus.Info("[RUNV] Connection interrupted while destroying")
		case COMMAND_RELEASE:
			logrus.Info("[RUNV] vm destroying, got release")
			ctx.reportVmShutdown()
		case EVENT_VM_TIMEOUT:
			logrus.Info("[RUNV] Device removing timeout")
			ctx.Close()
		default:
			unexpectedEventHandler(ctx, ev, "vm cleaning up")
		}
	}
}

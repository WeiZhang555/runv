package qemu

import (
	"os"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/lib/utils"
)

func watchDog(qc *QemuContext, hub chan hypervisor.VmEvent) {
	wdt := qc.wdt
	for {
		msg, ok := <-wdt
		if ok {
			switch msg {
			case "quit":
				logrus.Info("[RUNV] quit watch dog.")
				return
			case "kill":
				success := false
				if qc.process != nil {
					logrus.Infof("[RUNV] kill Qemu... %d", qc.process.Pid)
					if err := qc.process.Kill(); err == nil {
						success = true
					}
				} else {
					logrus.Warning("no process to be killed")
				}
				hub <- &hypervisor.VmKilledEvent{Success: success}
				return
			}
		} else {
			logrus.Info("[RUNV] chan closed, quit watch dog.")
			break
		}
	}
}

func (qc *QemuContext) watchPid(pid int, hub chan hypervisor.VmEvent) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	qc.process = proc
	go watchDog(qc, hub)

	return nil
}

// launchQemu run qemu and wait it's quit, includes
func launchQemu(qc *QemuContext, ctx *hypervisor.VmContext) {
	qemu := qc.driver.executable
	if qemu == "" {
		ctx.Hub <- &hypervisor.VmStartFailEvent{Message: "can not find qemu executable"}
		return
	}

	args := qc.arguments(ctx)

	logrus.Info("[RUNV] cmdline arguments: ", strings.Join(args, " "))

	pid, err := utils.ExecInDaemon(qemu, append([]string{"qemu-system-x86_64"}, args...))
	if err != nil {
		//fail to daemonize
		logrus.Error("%v", err)
		ctx.Hub <- &hypervisor.VmStartFailEvent{Message: "try to start qemu failed"}
		return
	}

	logrus.Infof("[RUNV] starting daemon with pid: %d", pid)

	err = ctx.DCtx.(*QemuContext).watchPid(int(pid), ctx.Hub)
	if err != nil {
		logrus.Error("watch qemu process failed")
		ctx.Hub <- &hypervisor.VmStartFailEvent{Message: "watch qemu process failed"}
		return
	}
}

func associateQemu(ctx *hypervisor.VmContext) {
	go watchDog(ctx.DCtx.(*QemuContext), ctx.Hub)
}

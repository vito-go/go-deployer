package main

import (
	"flag"
	"log"
	"os"

	"github.com/vito-go/go-deployer/agent"
)

// terminal-agent: 独立常驻的 shell 入口。不管业务、不 bind 端口,
// 只为了在业务进程(mychat-server 等)部署/崩溃/端口冲突时,
// dashboard 依然能点进 PTY 手动操作这台机器。
//
// 跨平台:Windows 用 ConPTY,Linux/macOS 用 creack/pty,agent 包已分派。
func main() {
	server := flag.String("server", "", "controlplane host:port, e.g. deployer-cdn.myproxy.life:2053")
	token := flag.String("token", "", "agent token")
	certFP := flag.String("cert-fp", "", "controlplane cert fingerprint")
	svc := flag.String("service", "terminal", "service name shown in dashboard")
	group := flag.String("group", "", "group (optional)")
	flag.Parse()

	if *server == "" {
		log.Fatal("--server required")
	}

	hostname, _ := os.Hostname()
	log.Printf("[terminal-agent] starting: server=%s service=%s host=%s", *server, *svc, hostname)

	agent.Register(agent.Config{
		ServerHost:  *server,
		ServiceName: *svc,
		Group:       *group,
		Port:        0,
		Token:       *token,
		CertFP:      *certFP,
	})

	log.Printf("[terminal-agent] registered, blocking forever (Ctrl-C to exit)")
	select {}
}

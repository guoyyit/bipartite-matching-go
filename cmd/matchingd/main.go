// matchingd 是二分图匹配服务的本地可执行入口。
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/guoyyit/bipartite-matching-go"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址（仅本地回环）")
	dataDir := flag.String("data", "data", "本地持久化目录（WAL 与快照）")
	flag.Parse()

	svc, err := matching.NewService(*dataDir)
	if err != nil {
		log.Fatalf("open service: %v", err)
	}
	defer svc.Close()

	srv := matching.NewServer(svc)
	log.Printf("bipartite matching service listening on %s (data=%s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatalf("server: %v", err)
	}
}

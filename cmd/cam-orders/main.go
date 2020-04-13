package main

import (
	"flag"	
	"github.com/dynamicgo/config"
	"github.com/dynamicgo/slf4go"
	orderservice "github.com/camlabs/camorder"
	_ "github.com/lib/pq"
)

var logger = slf4go.Get("cam-order-service")
var configpath = flag.String("conf", "./config.json", "cam order service config file")

func main() {

	logger.Debug("version 1.0.0")

	flag.Parse()

	camcnf, err := config.NewFromFile(*configpath)

	if err != nil {
		logger.ErrorF("load cam config err , %s", err)
		return
	}

	//创建订单服务
	server, err := orderservice.NewHTTPServer(camcnf)

	if err != nil {
		logger.ErrorF("create http server err , %s", err)
		return
	}

	server.Run()
}

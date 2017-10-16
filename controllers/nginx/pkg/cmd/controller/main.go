/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
)

func main() {
	// start a new nginx controller
	ngx := newNGINXController()

	//用来接收终止信号
	go handleSigterm(ngx)
	// start the controller
	ngx.Start()
	// wait
	glog.Infof("shutting down Ingress controller...")
	for {
		glog.Infof("Handled quit, awaiting pod deletion")
		time.Sleep(30 * time.Second)
	}
}

func handleSigterm(ngx *NGINXController) {
	signalChan := make(chan os.Signal, 1)
	//一旦有程序终止的信号，就会传送到signalChan
	signal.Notify(signalChan, syscall.SIGTERM)
	//等待程序终止的信号
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := ngx.Stop(); err != nil {
		glog.Infof("Error during shutdown %v", err)
		exitCode = 1
	}

	glog.Infof("Exiting with %v", exitCode)
	os.Exit(exitCode)
}

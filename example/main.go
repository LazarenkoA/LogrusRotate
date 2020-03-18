package main

import (
	"LogrusWriter"
	"github.com/sirupsen/logrus"
	"time"
)

type RotateConf struct {
}

func main() {
	lw := new(logrusRotate.Rotate).Construct()
	defer lw.Start(5, new(RotateConf))()

	timerChange := time.NewTicker(time.Minute * 5)
	for range timerChange.C {
		logrus.Info("Запись")
	}
}


func (w *RotateConf) LogDir() string {
	return "D:\\GoMy\\src\\LogrusWriter\\log"
}

func (w *RotateConf) FormatDir() string {
	return "02.01.2006"
}

func (w *RotateConf) FormatFile() string {
	return "15"
}
func (w *RotateConf) TTLLogs() int {
	return 1
}

func (w *RotateConf) TimeRotate() int {
	return 1
}
package logrusRotate

import (
	"context"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type IlogrusRotate interface {
	// Корневой каталог логов
	LogDir() string

	// Формат времени каталога логов, например "02.01.2006"
	FormatDir() string

	// Формат времени файла логов, например "15.04.05"
	FormatFile() string

	// Время жизни всех логов (в часах). Сколько часов могут жить логи (при превышении логи удаляются).
	// Не может быть меньше или равен значению TimeRotate
	TTLLogs() int

	// Время ротации логов (в часах). Через сколько часов создасться новый файл, минимальное время 1 час.
	TimeRotate() int
}

type Rotate struct {
	watcher     *fsnotify.Watcher
	ttltimer    *time.Ticker
	timerChange *time.Ticker
	ctx         context.Context
	dirPath     string
}

func (this *Rotate) createDir(dir string, forceRecreate chan string) {
	defer func() {
		if e := recover(); e != nil {
			logrus.WithError(fmt.Errorf("%v", e)).
				WithField("dir", dir).
				Error("Произошла ошибка при создании каталога")
		}
	}()

	logrus.Debugln("Создаем каталог", dir)
	var cansel context.CancelFunc

	this.ctx, cansel = context.WithCancel(context.Background())
	this.dirPath = dir

	actions := make(map[fsnotify.Op]func(string))
	actions[fsnotify.Remove] = func(Delfile string) {
		if stat, err := os.Stat(Delfile); !os.IsNotExist(err) && stat.IsDir() {
			defer cansel() // что бы закрылся хук т.к. нам он уже не нужен, каталог то удален
		}

		// Если удален активный файл логов, то создаем новый
		dir, _ := filepath.Split(logrus.StandardLogger().Out.(*os.File).Name())
		if logrus.StandardLogger().Out.(*os.File).Name() == Delfile || dir[:len(dir)-1] == Delfile {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				this.createDir(dir, forceRecreate) // вызываем createDir, что бы опять установился хук на новый каталог
			}
			forceRecreate <- dir
		}
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.Mkdir(dir, os.ModePerm); err != nil {
			logrus.WithError(err).Errorln("Ошибка создания каталога ", dir)
		}
	}

	go this.NewHook(actions)
}

func (this *Rotate) Start(LogLevel int, logW IlogrusRotate) func() {

	if logW.TTLLogs() < logW.TimeRotate() {
		panic("TTLLogs не может быть меньше или равен значению TimeRotate")
	}

	forceRecreate := make(chan string)
	if _, err := os.Stat(logW.LogDir()); os.IsNotExist(err) {
		this.createDir(logW.LogDir(), forceRecreate)
	}

	dir := filepath.Join(logW.LogDir(), time.Now().Format(logW.FormatDir()))
	this.createDir(dir, forceRecreate)

	tmp, _ := os.OpenFile(filepath.Join(dir, "Log_"+time.Now().Format(logW.FormatFile())), os.O_CREATE|os.O_WRONLY, os.ModePerm)
	logrus.SetOutput(tmp)

	this.timerChange = time.NewTicker(time.Minute)
	timeStart := time.Now()
	currentMinute := time.Now().Minute()  // нам нужно понимать сколько прошло минут текущего часа
	go func() {
		for {
			createfile := func(dir string) {
				defer func() {
					if e := recover(); e != nil {
						logrus.WithError(fmt.Errorf("%v", e)).
							WithField("dir", dir).
							Error("Произошла ошибка при создании файла")
					}
				}()

				newFileName := filepath.Join(dir, "Log_"+time.Now().Format(logW.FormatFile()))
				if _, err := os.Stat(newFileName); os.IsExist(err) {
					return
				}

				Log, _ := os.OpenFile(newFileName, os.O_CREATE|os.O_WRONLY, os.ModePerm)
				oldFile := logrus.StandardLogger().Out.(*os.File)
				logrus.SetOutput(Log)
				this.DeleleEmptyFile(oldFile)
			}

			select {
			case <-this.timerChange.C:
				if time.Since(timeStart).Minutes() + float64(currentMinute) < float64(logW.TimeRotate() * 60) {
					continue
				}
				timeStart = time.Now()
				currentMinute = time.Now().Minute()

				dir := filepath.Join(logW.LogDir(), time.Now().Format(logW.FormatDir()))
				this.createDir(dir, forceRecreate)
				createfile(dir)
			case dir := <-forceRecreate:
				createfile(dir)
			}

		}
	}()

	// очистка старых файлов и пустых каталогов
	this.ttltimer = time.NewTicker(time.Minute * 50)
	go func() {
		for range this.ttltimer.C {
			filepath.Walk(this.dirPath, func(path string, info os.FileInfo, err error) error {
				// Проверяем существование т.к. может быть удален другой горутиной
				if _, err := os.Stat(path); os.IsNotExist(err) {
					return nil
				}

				if !info.IsDir() {
					diff := time.Since(info.ModTime()).Hours()
					if diff > float64(logW.TTLLogs()) {
						if err := os.Remove(path); err != nil {
							logrus.WithError(err).WithField("Файл", path).Error("Ошибка удаления файла")
						}
					}
				} else {
					// Очистка пустых каталогов
					if dir, err := os.OpenFile(path, os.O_RDONLY, os.ModeDir); err == nil {
						this.DeleleEmptyFile(dir)
					} else {
						return err
					}
				}

				return nil
			})
		}
	}()

	if LogLevel > 0 {
		logrus.SetLevel(logrus.Level(LogLevel))
	}

	return this.Destroy
}

func (this *Rotate) Construct() *Rotate {
	this.watcher, _ = fsnotify.NewWatcher()
	//this.mu = new(sync.Mutex)

	return this
}

func (this *Rotate) Destroy() {
	this.timerChange.Stop()
	this.ttltimer.Stop()
	this.watcher.Close()

	this.DeleleEmptyFile(logrus.StandardLogger().Out.(*os.File))
}

func (this *Rotate) DeleleEmptyFile(file *os.File) {
	defer func() {
		// если файл все еще есть, значит он не пуст, просто закрываем его
		if _, err := file.Stat(); !os.IsNotExist(err) {
			file.Close()
		}
	}()

	if file == nil {
		return
	}
	// Если файл пустой, удаляем его. что бы не плодил кучу файлов
	info, err := file.Stat()
	if err != nil || info == nil {
		return
	}

	if info.Size() == 0 && !info.IsDir() {
		file.Close()

		if err := os.Remove(file.Name()); err != nil {
			logrus.WithError(err).WithField("Файл", file.Name()).Error("Ошибка удаления файла")
		}
	}

	var dirPath string
	// Для каталога, если пустой, то зачем он нам
	if !info.IsDir() {
		dirPath, _ = filepath.Split(file.Name())
	} else {
		dirPath = file.Name()
		file.Close()
	}

	// Если в текущем каталоге нет файлов, пробуем удалить его
	files, err := ioutil.ReadDir(dirPath)
	if err != nil {
		logrus.WithError(err).WithField("Каталог", dirPath).Error("Ошибка получения списка файлов в каталоге")
		return
	}

	if len(files) == 0 {
		os.Remove(dirPath)
	}

}

// Хук нужен для отслеживания удаления файлов логов, что бы тут же создать новый
// в linux можно удалить даже когда открыт дескриптор на файл
func (this *Rotate) NewHook(actions map[fsnotify.Op]func(string)) {
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			select {
			case event, ok := <-this.watcher.Events:
				if !ok {
					return
				}
				for fsnotifyType, action := range actions {
					if event.Op&fsnotifyType == fsnotifyType {
						action(event.Name)
					}
				}
			case err, ok := <-this.watcher.Errors:
				if !ok {
					return
				}
				log.Println("Ошибка мониторинга директории:", err)
			case <-this.ctx.Done():
				return
			}
		}
	}()

	err := this.watcher.Add(this.dirPath)
	if err != nil {
		logrus.WithError(err).Errorf("Не удалось установить мониторинг за каталогом %q", this.dirPath)
	}

	wg.Wait()
}


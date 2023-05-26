package logrusRotate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/matryer/resync"
	"github.com/sirupsen/logrus"
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
	ttlTimer    *time.Ticker
	timerChange *time.Ticker
	dirPath     atomic.Value
	watch       *watcher
}

type watcher struct {
	*fsnotify.Watcher

	actionsType fsnotify.Op
	one         resync.Once
	handler     func(*watcher, string)
	dirPath     string
	ctx         context.Context
	cancel      context.CancelFunc
}

func newWatcher(actionsType fsnotify.Op, dirPath string, handler func(*watcher, string)) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w, _ := fsnotify.NewWatcher()

	return &watcher{
		actionsType: actionsType,
		handler:     handler,
		ctx:         ctx,
		cancel:      cancel,
		Watcher:     w,
		dirPath:     dirPath,
	}
}

// Хук нужен для отслеживания удаления файлов логов, что бы тут же создать новый
// в linux можно удалить даже когда открыт дескриптор на файл
func (a *watcher) runHook() {
	logrus.WithField("dirPath", a.dirPath).Info("add hook")
	defer func() {
		logrus.WithField("dirPath", a.dirPath).Info("Close watcher")
	}()

	err := a.Watcher.Add(a.dirPath)
	if err != nil {
		logrus.WithError(err).Errorf("Не удалось установить мониторинг за каталогом %q", a.dirPath)
	}

	for {
		select {
		case event, ok := <-a.Watcher.Events:
			if !ok {
				return
			}

			if event.Op == a.actionsType {
				a.handler(a, event.Name)
			}
		case err := <-a.Watcher.Errors:
			logrus.WithError(err).Error("Ошибка мониторинга директории")
			return
		case <-a.ctx.Done():
			return
		}
	}
}

func (a *watcher) Cancel() {
	a.cancel()
	a.Close()
}

// Для обращений к logrus
func StandardLogger() *logrus.Logger {
	return logrus.StandardLogger()
}

func (r *Rotate) createDir(conf IlogrusRotate, forceRecreate chan string) {
	defer func() {
		if e := recover(); e != nil {
			logrus.WithError(fmt.Errorf("%v", e)).Error("Произошла ошибка при создании каталога")
		}
	}()

	r.dirPath.Store(filepath.Join(conf.LogDir(), time.Now().Format(conf.FormatDir())))
	if _, err := os.Stat(r.dirPath.Load().(string)); !os.IsNotExist(err) {
		return
	}

	logrus.Debugln("Создаем новый каталог", r.dirPath)
	if err := os.MkdirAll(r.dirPath.Load().(string), os.ModePerm); err != nil {
		logrus.WithError(err).Errorln("Ошибка создания каталога ", r.dirPath)
	}

	r.watch = newWatcher(fsnotify.Remove, r.dirPath.Load().(string), func(w *watcher, currentFile string) {
		if currentFile == w.dirPath {
			defer w.Cancel() // что бы закрылся хук т.к. нам он уже не нужен, новый хук будет установлен на новый каталог
		}

		if currentFile != w.dirPath { // хук будет вызываться для каждого файла в каталоге, нам по сути нужен только текущий каталог в который пишутся логи
			return
		}

		go func() {
			r.createDir(conf, forceRecreate)           // вызываем createDir, что бы опять установился хук на новый каталог
			forceRecreate <- r.dirPath.Load().(string) // отправляем в канал для принудительного пересоздания файла
		}()
	})

	go r.watch.runHook()
}

func (r *Rotate) createFile(fileName string) (file *os.File) {
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		file, _ = os.Create(fileName)
	} else {
		file, _ = os.OpenFile(fileName, os.O_APPEND, os.ModeAppend)
	}
	return file
}

func (r *Rotate) Start(LogLevel int, conf IlogrusRotate) func() {
	if conf.TTLLogs() < conf.TimeRotate() {
		panic("TTLLogs не может быть меньше или равен значению TimeRotate")
	}

	forceRecreate := make(chan string)
	r.createDir(conf, forceRecreate)

	fpath := filepath.Join(r.dirPath.Load().(string), "Log_"+time.Now().Format(conf.FormatFile())+".log")
	logrus.SetOutput(r.createFile(fpath))

	r.timerChange = time.NewTicker(time.Minute)
	timeStart := time.Now()
	currentMinute := time.Now().Minute() // нам нужно понимать сколько прошло минут текущего часа
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

				newFileName := filepath.Join(dir, "Log_"+time.Now().Format(conf.FormatFile())+".log")
				if _, err := os.Stat(newFileName); !os.IsNotExist(err) {
					return
				}

				oldFile := logrus.StandardLogger().Out.(*os.File)
				logrus.SetOutput(r.createFile(newFileName))
				oldFile.Close()
			}

			select {
			case <-r.timerChange.C:
				if time.Since(timeStart).Minutes()+float64(currentMinute) < float64(conf.TimeRotate()*60) {
					continue
				}
				timeStart = time.Now()
				currentMinute = time.Now().Minute()

				r.createDir(conf, forceRecreate)
				createfile(r.dirPath.Load().(string))
			case dir := <-forceRecreate:
				createfile(dir)
			}

		}
	}()

	// очистка старых файлов и пустых каталогов
	r.ttlTimer = time.NewTicker(time.Minute)
	go func() {
		for range r.ttlTimer.C {
			filepath.Walk(conf.LogDir(), func(path string, info os.FileInfo, err error) error {
				if !info.IsDir() {
					diff := time.Since(info.ModTime()).Hours()
					if diff > float64(conf.TTLLogs()) {
						if err := os.RemoveAll(path); err != nil {
							logrus.WithError(err).WithField("Файл", path).Error("Ошибка удаления файла")
						}
					}
				} else {
					// Очистка пустых каталогов
					if dir, err := os.OpenFile(path, os.O_RDONLY, os.ModeDir); err == nil {
						r.DeleteEmptyFile(dir)
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

	return r.Destroy
}

func (r *Rotate) Construct() *Rotate {
	// todo
	return r
}

func (r *Rotate) Destroy() {
	r.timerChange.Stop()
	r.ttlTimer.Stop()

	// r.DeleleEmptyFile(logrus.StandardLogger().Out.(*os.File))
}

func (r *Rotate) DeleteEmptyFile(file *os.File) {
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
	if !info.IsDir() {
		dirPath, _ = filepath.Split(file.Name())
	} else {
		dirPath = file.Name()
		file.Close()
	}

	// Если в текущем каталоге нет файлов, пробуем удалить его
	if files, err := os.ReadDir(dirPath); err != nil {
		logrus.WithError(err).WithField("Каталог", dirPath).Error("Ошибка получения списка файлов в каталоге")
		return
	} else if len(files) == 0 {
		r.watch.Cancel() // перед удалением отменяем хук, т.к. хук нужен как защита от удаления текущего каталока (в ллинукс можно удалить каталог с файлами в который в данным момент пишутся логи)
		if err := os.RemoveAll(dirPath); err != nil {
			logrus.WithError(err).WithField("dirPath", dirPath).Error("Ошибка удаления каталога")
		}
	}
}

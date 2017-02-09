// Copyright 2016 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package thinkgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henrylee2cn/thinkgo/acceptencoder"
	"github.com/henrylee2cn/thinkgo/apiware"
	"github.com/henrylee2cn/thinkgo/logging"
	"github.com/henrylee2cn/thinkgo/utils"
)

type (
	// GlobalVariables defines the global frames, configuration, function and so on.
	GlobalVariables struct {
		// the list of applications that have been created.
		frames     []*Framework
		framesLock sync.RWMutex
		// global config
		config GlobalConfig
		// Error replies to the request with the specified error message and HTTP code.
		// It does not otherwise end the request; the caller should ensure no further
		// writes are done to response.
		// The error message should be plain text.
		errorFunc ErrorFunc
		// The following is only for the APIHandler
		binderrorFunc BinderrorFunc
		// Decode params from request body.
		bodydecoder apiware.Bodydecoder
		// When the APIHander's parameter name (struct tag) is unsetted,
		// it is mapped from the structure field name by default.
		// If `paramNameMapper` is nil, use snake style.
		// If the APIHander's parameter binding fails, the default handler is invoked
		paramNameMapper apiware.ParamNameMapper
		// global file cache system manager
		fsManager *FileServerManager
		// Render is a custom thinkgo template renderer using pongo2.
		render *Render

		// The path for the upload files.
		// When does not have a custom route, the route is automatically created.
		upload PresetStatic

		// The path for the static files.
		// When does not have a custom route, the route is automatically created.
		static PresetStatic

		// The path for the log files
		logDir string

		syslog *logging.Logger
		bizlog *logging.Logger

		// finalizer is called after the services shutdown.
		finalizer func(context.Context) error

		graceOnce sync.Once
	}
	// PresetStatic is the system default static file routing information
	PresetStatic struct {
		root       string
		nocompress bool
		nocache    bool
		handlers   []Handler
	}
)

var (
	// global is the global configuration, functions and so on.
	global = func() *GlobalVariables {
		global := &GlobalVariables{
			frames:          []*Framework{},
			config:          globalConfig,
			errorFunc:       defaultErrorFunc,
			bodydecoder:     defaultBodydecoder,
			binderrorFunc:   defaultBinderrorFunc,
			paramNameMapper: defaultParamNameMapper,
			fsManager: newFileServerManager(
				globalConfig.Cache.SizeMB*1024*1024,
				globalConfig.Cache.Expire,
				globalConfig.Cache.Enable,
				globalConfig.Gzip.Enable,
			),
			upload: defaultUpload,
			static: defaultStatic,
			logDir: defaultLogDir,
		}
		if globalConfig.Cache.Enable {
			global.render = newRender(func(name string) (http.File, error) {
				return global.fsManager.Open(name, "", false)
			})
		} else {
			global.render = newRender(nil)
		}
		global.initLogger()
		return global
	}()
	defaultErrorFunc = func(ctx *Context, errStr string, status int) {
		if status >= 500 {
			ctx.Log().Error(errStr)
		}
		statusText := http.StatusText(status)
		if len(errStr) > 0 {
			errStr = `<br><p><b style="color:red;">[ERROR]</b> <pre>` + errStr + `</pre></p>`
		}
		ctx.W.Header().Set(HeaderXContentTypeOptions, nosniff)
		ctx.HTML(status, fmt.Sprintf("<html>\n"+
			"<head><title>%d %s</title></head>\n"+
			"<body bgcolor=\"white\">\n"+
			"<center><h1>%d %s</h1></center>\n"+
			"<hr>\n<center>thinkgo/%s</center>\n%s\n</body>\n</html>\n",
			status, statusText, status, statusText, VERSION, errStr),
		)
	}
	// The default body decoder is json format decoding
	defaultBodydecoder = func(dest reflect.Value, body []byte) error {
		var err error
		if dest.Kind() == reflect.Ptr {
			err = json.Unmarshal(body, dest.Interface())
		} else {
			err = json.Unmarshal(body, dest.Addr().Interface())
		}
		return err
	}
	defaultBinderrorFunc = func(ctx *Context, err error) {
		ctx.String(http.StatusBadRequest, "%v", err)
	}
	defaultParamNameMapper = utils.SnakeString
	// The default path for the upload files
	defaultUpload = PresetStatic{
		root: "./upload/",
	}
	// The default path for the static files
	defaultStatic = PresetStatic{
		root: "./static/",
	}
	// The default path for the log files
	defaultLogDir = "./log/"
)

func init() {
	fmt.Println(banner[1:])
	global.syslog.Criticalf("The PID of the current process is %d", os.Getpid())
	if global.config.warnMsg != "" {
		Warning(global.config.warnMsg)
		global.config.warnMsg = ""
	}
	// init file cache
	acceptencoder.InitGzip(global.config.Gzip.MinLength, global.config.Gzip.CompressLevel, global.config.Gzip.Methods)
}

func addFrame(frame *Framework) {
	global.framesLock.Lock()
	defer global.framesLock.Unlock()
	name := frame.NameWithVersion()
	for _, v := range global.frames {
		if v.NameWithVersion() == name {
			frame.Log().Panicf("frame %s is registered repeatedly", name)
		}
	}
	global.frames = append(global.frames, frame)
}

// AllFrames returns the list of applications that have been created.
func AllFrames() []*Framework {
	global.framesLock.RLock()
	defer global.framesLock.RUnlock()
	return global.frames
}

// GetFrame returns the specified frame instance by name and version.
func GetFrame(name string, version ...string) (*Framework, bool) {
	if len(version) > 0 && len(version[0]) > 0 {
		name = name + "_" + version[0]
	}
	global.framesLock.RLock()
	defer global.framesLock.RUnlock()
	for _, frame := range global.frames {
		if frame.NameWithVersion() == name {
			return frame, true
		}
	}
	return nil, false
}

// Run starts all web services.
func Run() {
	global.framesLock.Lock()
	for _, frame := range global.frames {
		if !frame.Running() {
			go frame.run()
			time.Sleep(time.Second)
		}
	}
	global.framesLock.Unlock()
	global.graceOnce.Do(func() {
		graceSignal()
	})
}

// Running returns whether the frame service is running.
func Running(name string, version ...string) bool {
	frame, ok := GetFrame(name, version...)
	if !ok {
		return false
	}
	return frame.Running()
}

// SetFinalizer sets the function which is called after the services shutdown.
func SetFinalizer(finalizer func(context.Context) error) {
	global.finalizer = finalizer
}

const (
	// SHUTDOWN_TIMEOUT the time-out period for closing the service
	SHUTDOWN_TIMEOUT = 1 * time.Minute
)

// Shutdown closes all the frame services gracefully.
func Shutdown(timeout ...time.Duration) {
	Print("\x1b[46m[SYS]\x1b[0m shutting down servers...")
	global.framesLock.Lock()
	defer global.framesLock.Unlock()
	// shut down gracefully, but wait no longer than d before halting
	var d = SHUTDOWN_TIMEOUT
	if len(timeout) > 0 {
		d = timeout[0]
	}
	ctxTimeout, _ := context.WithTimeout(context.Background(), d)
	count := new(sync.WaitGroup)
	var flag int32 = 1
	for _, frame := range global.frames {
		count.Add(1)
		go func(fm *Framework) {
			graceful := fm.shutdown(ctxTimeout)
			if !graceful {
				atomic.StoreInt32(&flag, 0)
			}
			count.Done()
		}(frame)
	}
	count.Wait()
	if global.finalizer != nil {
		if err := global.finalizer(ctxTimeout); err != nil {
			flag = 0
			Error("[finalizer]", err.Error())
		}
	}
	if flag == 1 {
		Print("\x1b[46m[SYS]\x1b[0m servers are shutted down gracefully.")
	} else {
		Print("\x1b[46m[SYS]\x1b[0m servers are shutted down, but not gracefully.")
	}
	CloseLog()
}

// HandleError calls the default error handler.
func HandleError(ctx *Context, errStr string, status int) {
	global.errorFunc(ctx, errStr, status)
}

// SetErrorFunc sets the global default `ErrorFunc` function.
func SetErrorFunc(errorFunc ErrorFunc) {
	if errorFunc == nil {
		global.errorFunc = defaultErrorFunc
	} else {
		global.errorFunc = errorFunc
	}
}

// DecodeBody decodes params from request body.
func DecodeBody(dest reflect.Value, body []byte) error {
	return global.bodydecoder(dest, body)
}

// SetBodydecoder sets the global default `Bodydecoder` function.
func SetBodydecoder(bodydecoder apiware.Bodydecoder) {
	if bodydecoder == nil {
		global.bodydecoder = defaultBodydecoder
	} else {
		global.bodydecoder = bodydecoder
	}
}

// HandleBinderror calls the default parameter binding failure handler.
func HandleBinderror(ctx *Context, err error) {
	global.binderrorFunc(ctx, err)
}

// SetBinderrorFunc sets the global default `BinderrorFunc` function.
func SetBinderrorFunc(binderrorFunc BinderrorFunc) {
	if binderrorFunc == nil {
		global.binderrorFunc = defaultBinderrorFunc
	} else {
		global.binderrorFunc = binderrorFunc
	}
}

// MapParamName maps the APIHander's parameter name from the structure field.
// When the APIHander's parameter name (struct tag) is unsetted,
// it is mapped from the structure field name by default.
// If `paramNameMapper` is nil, use snake style.
func MapParamName(fieldName string) (paramName string) {
	return global.paramNameMapper(fieldName)
}

// SetParamNameMapper sets the global default `ParamNameMapper` function.
func SetParamNameMapper(paramNameMapper apiware.ParamNameMapper) {
	if paramNameMapper == nil {
		global.paramNameMapper = defaultParamNameMapper
	} else {
		global.paramNameMapper = paramNameMapper
	}
}

// GetRender returns a custom thinkgo template renderer using pongo2.
func GetRender() *Render {
	return global.render
}

// RenderVar sets the global template variable, function or pongo2.FilterFunction for pongo2 render.
func RenderVar(name string, v interface{}) {
	global.render.TemplateVar(name, v)
}

// LogDir returns logs folder path with a slash at the end
func LogDir() string {
	return global.logDir
}

// SetUpload sets upload folder path such as `./upload/`
// with a slash `/` at the end.
// note: it should be called before Run()
func SetUpload(dir string, nocompress bool, nocache bool, handlers ...Handler) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	global.upload = PresetStatic{
		root:       dir,
		nocompress: nocompress,
		nocache:    nocache,
		handlers:   handlers,
	}
}

// UploadDir returns upload folder path with a slash at the end
func UploadDir() string {
	return global.upload.root
}

// SetStatic sets static folder path, such as `./staic/`
// with a slash `/` at the end.
// note: it should be called before Run()
func SetStatic(dir string, nocompress bool, nocache bool, handlers ...Handler) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	global.static = PresetStatic{
		root:       dir,
		nocompress: nocompress,
		nocache:    nocache,
		handlers:   handlers,
	}
}

// StaticDir returns static folder path with a slash at the end
func StaticDir() string {
	return global.static.root
}

// CloseLog closes global loggers.
func CloseLog() {
	global.bizlog.Close()
	global.syslog.Close()
}

// Fatal is equivalent to l.Critical(fmt.Sprint()) followed by a call to os.Exit(1).
func Fatal(args ...interface{}) {
	global.bizlog.Fatal(args...)
}

// Fatalf is equivalent to l.Critical followed by a call to os.Exit(1).
func Fatalf(format string, args ...interface{}) {
	global.bizlog.Fatalf(format, args...)
}

// Panic is equivalent to l.Critical(fmt.Sprint()) followed by a call to panic().
func Panic(args ...interface{}) {
	global.bizlog.Panic(args...)
}

// Panicf is equivalent to l.Critical followed by a call to panic().
func Panicf(format string, args ...interface{}) {
	global.bizlog.Panicf(format, args...)
}

// Critical logs a message using CRITICAL as log level.
func Critical(args ...interface{}) {
	global.bizlog.Critical(args...)
}

// Criticalf logs a message using CRITICAL as log level.
func Criticalf(format string, args ...interface{}) {
	global.bizlog.Criticalf(format, args...)
}

// Error logs a message using ERROR as log level.
func Error(args ...interface{}) {
	global.bizlog.Error(args...)
}

// Errorf logs a message using ERROR as log level.
func Errorf(format string, args ...interface{}) {
	global.bizlog.Errorf(format, args...)
}

// Warning logs a message using WARNING as log level.
func Warning(args ...interface{}) {
	global.bizlog.Warning(args...)
}

// Warningf logs a message using WARNING as log level.
func Warningf(format string, args ...interface{}) {
	global.bizlog.Warningf(format, args...)
}

// Notice logs a message using NOTICE as log level.
func Notice(args ...interface{}) {
	global.bizlog.Notice(args...)
}

// Noticef logs a message using NOTICE as log level.
func Noticef(format string, args ...interface{}) {
	global.bizlog.Noticef(format, args...)
}

// Info logs a message using INFO as log level.
func Info(args ...interface{}) {
	global.bizlog.Info(args...)
}

// Infof logs a message using INFO as log level.
func Infof(format string, args ...interface{}) {
	global.bizlog.Infof(format, args...)
}

// Debug logs a message using DEBUG as log level.
func Debug(args ...interface{}) {
	global.bizlog.Debug(args...)
}

// Debugf logs a message using DEBUG as log level.
func Debugf(format string, args ...interface{}) {
	global.bizlog.Debugf(format, args...)
}

// Print logs a message using CRITICAL as log level, only with time prefix.
func Print(args ...interface{}) {
	global.syslog.Critical(args...)
}

// Printf logs a message using CRITICAL as log level, only with time prefix.
func Printf(format string, args ...interface{}) {
	global.syslog.Criticalf(format, args...)
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/howeyc/fsnotify"

	"dlib/dbus"
	"dlib/gio-2.0"
	"dlib/glib-2.0"
)

const (
	_OBJECT = "com.deepin.SessionManager"
	_PATH   = "/com/deepin/StartManager"
	_INTER  = "com.deepin.StartManager"
)

const (
	_AUTOSTART    = "autostart"
	DESKTOP_ENV   = "Deepin"
	HiddenKey     = "Hidden"
	OnlyShowInKey = "OnlyShowIn"
	NotShowInKey  = "NotShowIn"
	TryExecKey    = "TryExec"
)

type StartManager struct {
	userAutostartPath string
	AutostartChanged  func(string, string)
}

func (m *StartManager) GetDBusInfo() dbus.DBusInfo {
	return dbus.DBusInfo{_OBJECT, _PATH, _INTER}
}

func (m *StartManager) Launch(name string) bool {
	list := make([]*gio.File, 0)
	err := launch(name, list)
	if err != nil {
		fmt.Println(err)
	}
	return err == nil
}

type AutostartInfo struct {
	renamed    chan bool
	created    chan bool
	notRenamed chan bool
	notCreated chan bool
}

const (
	AutostartAdded   string = "added"
	AutostartDeleted string = "deleted"
)

func (m *StartManager) emitAutostartChanged(name, status string, info map[string]AutostartInfo) {
	m.AutostartChanged(status, name)
	delete(info, name)
}

func (m *StartManager) autostartHandler(ev *fsnotify.FileEvent, name string, info map[string]AutostartInfo) {
	// fmt.Println(ev)
	if _, ok := info[name]; !ok {
		info[name] = AutostartInfo{
			make(chan bool),
			make(chan bool),
			make(chan bool),
			make(chan bool),
		}
	}
	if ev.IsRename() {
		select {
		case <-info[name].renamed:
		default:
		}
		go func() {
			select {
			case <-info[name].notRenamed:
				return
			case <-time.After(time.Second):
				<-info[name].renamed
				m.emitAutostartChanged(name, AutostartDeleted, info)
				// fmt.Println(AutostartDeleted)
			}
		}()
		info[name].renamed <- true
	} else if ev.IsCreate() {
		go func() {
			select {
			case <-info[name].renamed:
				info[name].notRenamed <- true
				info[name].renamed <- true
			default:
			}
			select {
			case <-info[name].notCreated:
				return
			case <-time.After(time.Second):
				<-info[name].created
				m.emitAutostartChanged(name, AutostartAdded, info)
				// fmt.Println("create", AutostartAdded)
			}
		}()
		info[name].created <- true
	} else if ev.IsModify() && !ev.IsAttrib() {
		go func() {
			select {
			case <-info[name].created:
				info[name].notCreated <- true
			}
			select {
			case <-info[name].renamed:
				// fmt.Println("modified")
				if m.isAutostart(name) {
					m.emitAutostartChanged(name, AutostartAdded, info)
				} else {
					m.emitAutostartChanged(name, AutostartDeleted, info)
				}
			default:
				m.emitAutostartChanged(name, AutostartAdded, info)
				// fmt.Println("modify", AutostartAdded)
			}
		}()
	} else if ev.IsAttrib() {
		go func() {
			select {
			case <-info[name].renamed:
				<-info[name].created
				info[name].notCreated <- true
			default:
			}
		}()
	} else if ev.IsDelete() {
		m.emitAutostartChanged(name, AutostartDeleted, info)
		// fmt.Println(AutostartDeleted)
	}
}

func (m *StartManager) eventHandler(watcher *fsnotify.Watcher) {
	info := map[string]AutostartInfo{}
	for {
		select {
		case ev := <-watcher.Event:
			name := path.Clean(ev.Name)
			basename := path.Base(name)
			matched, _ := path.Match(`[^#.]*.desktop`, basename)
			if matched {
				if _, ok := info[name]; !ok {
					info[name] = AutostartInfo{
						make(chan bool, 1),
						make(chan bool, 1),
						make(chan bool, 1),
						make(chan bool, 1),
					}
				}
				m.autostartHandler(ev, name, info)
			}
		case <-watcher.Error:
		}
	}
}

func (m *StartManager) listenAutostart() {
	watcher, _ := fsnotify.NewWatcher()
	for _, dir := range m.autostartDirs() {
		watcher.Watch(dir)
	}
	go m.eventHandler(watcher)
}

type ActionGroup struct {
	Action     string
	ActionName string
}

func (m *StartManager) ListActions(name string) []ActionGroup {
	actions := make([]ActionGroup, 0)
	var o *gio.DesktopAppInfo
	if path.IsAbs(name) {
		o = gio.NewDesktopAppInfoFromFilename(name)
	} else {
		o = gio.NewDesktopAppInfo(name)
	}
	if o == nil {
		return actions
	}
	defer o.Unref()

	for _, action := range o.ListActions() {
		actionName := o.GetActionName(action)
		// fmt.Printf("%s, \"%s\"\n", action, actionName)
		actions = append(actions, ActionGroup{action, actionName})
	}
	return actions
}

func (m *StartManager) LaunchAction(name, action string) bool {
	var o *gio.DesktopAppInfo
	if path.IsAbs(name) {
		o = gio.NewDesktopAppInfoFromFilename(name)
	} else {
		o = gio.NewDesktopAppInfo(name)
	}
	if o == nil {
		fmt.Println("Create DesktopAppInfo failed")
		return false
	}
	defer o.Unref()

	for _, actionGroup := range m.ListActions(name) {
		if action == actionGroup.Action {
			o.LaunchAction(action, nil)
			return true
		}
	}

	fmt.Printf("Has no such a key '%s'\n", action)
	return false
}

// filepath.Walk will walk through the whole directory tree
func scanDir(d string, fn func(path string, info os.FileInfo) error) {
	f, err := os.Open(d)
	if err != nil {
		// fmt.Println("scanDir", err)
		return
	}
	infos, err := f.Readdir(0)
	if err != nil {
		fmt.Println("scanDir", err)
		return
	}

	for _, info := range infos {
		if fn(d, info) != nil {
			break
		}
	}
}

func (m *StartManager) getUserAutostart(name string) string {
	return path.Join(m.getUserAutostartDir(), path.Base(name))
}

func (m *StartManager) isUserAutostart(name string) bool {
	if path.IsAbs(name) {
		if !Exist(name) {
			return false
		}
		return path.Dir(name) == m.getUserAutostartDir()
	} else {
		return Exist(path.Join(m.getUserAutostartDir(), name))
	}
}

func (m *StartManager) isHidden(file *gio.DesktopAppInfo) bool {
	return file.HasKey(HiddenKey) && file.GetBoolean(HiddenKey)
}

func showInDeepinAux(file *gio.DesktopAppInfo, keyname string) bool {
	s := file.GetString(keyname)
	if s == "" {
		return false
	}

	for _, env := range strings.Split(s, ";") {
		if strings.ToLower(env) == strings.ToLower(DESKTOP_ENV) {
			return true
		}
	}

	return false
}

func (m *StartManager) showInDeepin(file *gio.DesktopAppInfo) bool {
	if file.HasKey(NotShowInKey) {
		return !showInDeepinAux(file, NotShowInKey)
	} else if file.HasKey(OnlyShowInKey) {
		return showInDeepinAux(file, OnlyShowInKey)
	}

	return true
}

func findExec(_path, cmd string, exist chan<- bool) {
	found := false

	scanDir(_path, func(p string, info os.FileInfo) error {
		if !info.IsDir() && info.Name() == cmd {
			found = true
			return errors.New("Found it")
		}
		return nil
	})

	exist <- found
	return
}

func (m *StartManager) hasValidTryExecKey(file *gio.DesktopAppInfo) bool {
	// name := file.GetFilename()
	if !file.HasKey(TryExecKey) {
		// fmt.Println(name, "No TryExec Key")
		return true
	}

	cmd := file.GetString(TryExecKey)
	if cmd == "" {
		// fmt.Println(name, "TryExecKey is empty")
		return true
	}

	if path.IsAbs(cmd) {
		// fmt.Println(cmd, "is exist?", Exist(cmd))
		if !Exist(cmd) {
			return false
		}

		stat, err := os.Lstat(cmd)
		if err != nil {
			return false
		}

		return (stat.Mode().Perm() & 0111) != 0
	} else {
		paths := strings.Split(os.Getenv("PATH"), ":")
		exist := make(chan bool)
		for _, _path := range paths {
			go findExec(_path, cmd, exist)
		}

		for _ = range paths {
			if t := <-exist; t {
				return true
			}
		}

		return false
	}
}

func (m *StartManager) isAutostartAux(name string) bool {
	file := gio.NewDesktopAppInfoFromFilename(name)
	if file == nil {
		return false
	}
	defer file.Unref()

	return m.hasValidTryExecKey(file) && !m.isHidden(file) && m.showInDeepin(file)
}

func lowerBaseName(name string) string {
	return strings.ToLower(path.Base(name))
}

func (m *StartManager) isSystemStart(name string) bool {
	if path.IsAbs(name) {
		if !Exist(name) {
			return false
		}
		d := path.Dir(name)
		for i, dir := range m.autostartDirs() {
			if i == 0 {
				continue
			}
			if d == dir {
				return true
			}
		}
		return false
	} else {
		return Exist(m.getSysAutostart(name))
	}

}
func (m *StartManager) getSysAutostart(name string) string {
	sysPath := ""
	for i, d := range m.autostartDirs() {
		if i == 0 {
			continue
		}
		scanDir(d,
			func(p string, info os.FileInfo) error {
				if lowerBaseName(name) == strings.ToLower(info.Name()) {
					sysPath = path.Join(p,
						info.Name())
					return errors.New("Found it")
				}
				return nil
			},
		)
		if sysPath != "" {
			return sysPath
		}
	}
	return sysPath
}

func (m *StartManager) isAutostart(name string) bool {
	if !strings.HasSuffix(name, ".desktop") {
		return false
	}

	u := m.getUserAutostart(name)
	if Exist(u) {
		name = u
	} else {
		s := m.getSysAutostart(name)
		if s == "" {
			return false
		}
		name = s
	}

	return m.isAutostartAux(name)
}

func (m *StartManager) getAutostartApps(dir string) []string {
	apps := make([]string, 0)

	scanDir(dir, func(p string, info os.FileInfo) error {
		if !info.IsDir() {
			fullpath := path.Join(p, info.Name())
			if m.isAutostart(fullpath) {
				apps = append(apps, fullpath)
			}
		}
		return nil
	})

	return apps
}

func (m *StartManager) getUserAutostartDir() string {
	if m.userAutostartPath == "" {
		configPath := glib.GetUserConfigDir()
		m.userAutostartPath = path.Join(configPath, _AUTOSTART)
	}

	if !Exist(m.userAutostartPath) {
		err := os.MkdirAll(m.userAutostartPath, 0775)
		if err != nil {
			fmt.Println(fmt.Errorf("create user autostart dir failed: %s", err))
		}
	}

	return m.userAutostartPath
}

func (m *StartManager) autostartDirs() []string {
	// first is user dir.
	dirs := []string{
		m.getUserAutostartDir(),
	}

	for _, configPath := range glib.GetSystemConfigDirs() {
		_path := path.Join(configPath, _AUTOSTART)
		if Exist(_path) {
			dirs = append(dirs, _path)
		}
	}

	return dirs
}

func (m *StartManager) AutostartList() []string {
	apps := make([]string, 0)
	dirs := m.autostartDirs()
	for _, dir := range dirs {
		if Exist(dir) {
			apps = append(apps, m.getAutostartApps(dir)...)
		}
	}
	return apps
}

func (m *StartManager) doSetAutostart(name string, autostart bool) error {
	file := glib.NewKeyFile()
	defer file.Free()
	if ok, err := file.LoadFromFile(name, glib.KeyFileFlagsNone); !ok {
		fmt.Println(err)
		return err
	}

	file.SetBoolean(
		glib.KeyFileDesktopGroup,
		HiddenKey,
		!autostart,
	)
	fmt.Println("set autostart to", autostart)

	return saveKeyFile(file, name)
}

func (m *StartManager) setAutostart(name string, autostart bool) error {
	if !path.IsAbs(name) {
		file := gio.NewDesktopAppInfo(name)
		if file == nil {
			return errors.New("cannot create desktop file")
		}
		name = file.GetFilename()
		file.Unref()
	}
	// fmt.Println(name, "autostart:", m.isAutostart(name))
	if autostart == m.isAutostart(name) {
		fmt.Println("is already done")
		return nil
	}

	dst := name
	if !m.isUserAutostart(name) {
		// fmt.Println("not user's")
		dst = m.getUserAutostart(name)
		fmt.Println(dst)
		if !Exist(dst) {
			err := copyFile(name, dst, CopyFileNotKeepSymlink)
			if err != nil {
				return fmt.Errorf("copy file failed: %s", err)
			}
		}
	}

	return m.doSetAutostart(dst, autostart)
}

func (m *StartManager) AddAutostart(name string) bool {
	err := m.setAutostart(name, true)
	if err != nil {
		fmt.Println("AddAutostart", err)
		return false
	}
	return true
}

func (m *StartManager) RemoveAutostart(name string) bool {
	err := m.setAutostart(name, false)
	if err != nil {
		fmt.Println(err)
		return false
	}
	return true
}

func (m *StartManager) IsAutostart(name string) bool {
	if !path.IsAbs(name) {
		file := gio.NewDesktopAppInfo(name)
		if file == nil {
			fmt.Println(name, "is not a vaild desktop file.")
			return false
		}
		name = file.GetFilename()
		file.Unref()
	}
	return m.isAutostart(name)
}

func startStartManager() {
	gio.DesktopAppInfoSetDesktopEnv(DESKTOP_ENV)
	m := StartManager{}
	if err := dbus.InstallOnSession(&m); err != nil {
		fmt.Println("Install StartManager Failed:", err)
	}
	m.listenAutostart()
	for _, name := range m.AutostartList() {
		// fmt.Println(name)
		if debug {
			continue
		}
		go m.Launch(name)
		<-time.After(20 * time.Millisecond)
	}
}
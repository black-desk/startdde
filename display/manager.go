package display

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pkg.deepin.io/lib/dbusutil/gsprop"

	"github.com/davecgh/go-spew/spew"
	dbus "github.com/godbus/dbus"
	inputdevices "github.com/linuxdeepin/go-dbus-factory/com.deepin.system.inputdevices"
	ofdbus "github.com/linuxdeepin/go-dbus-factory/org.freedesktop.dbus"
	x "github.com/linuxdeepin/go-x11-client"
	"github.com/linuxdeepin/go-x11-client/ext/randr"
	"golang.org/x/xerrors"
	"pkg.deepin.io/dde/api/dxinput"
	"pkg.deepin.io/dde/startdde/display/brightness"
	"pkg.deepin.io/dde/startdde/display/utils"
	gio "pkg.deepin.io/gir/gio-2.0"
	"pkg.deepin.io/lib/dbusutil"
)

const (
	DisplayModeCustom uint8 = iota
	DisplayModeMirror
	DisplayModeExtend
	DisplayModeOnlyOne
	DisplayModeUnknow
)

const (
	ColorTemperatureModeNormal int32 = iota
	ColorTemperatureModeAuto
	ColorTemperatureModeManual
)

const (
	gsSchemaDisplay  = "com.deepin.dde.display"
	gsKeyDisplayMode = "display-mode"
	gsKeyBrightness  = "brightness"
	gsKeySetter      = "brightness-setter"
	gsKeyMapOutput   = "map-output"
	//gsKeyPrimary     = "primary"
	gsKeyCustomMode             = "current-custom-mode"
	gsKeyColorTemperatureMode   = "color-temperature-mode"
	gsKeyColorTemperatureManual = "color-temperature-manual"
	customModeDelim             = "+"
	monitorsIdDelimiter         = ","

	cmdTouchscreenDialogBin = "/usr/lib/deepin-daemon/dde-touchscreen-dialog"
)

const (
	priorityEDP = iota
	priorityDP
	priorityHDMI
	priorityDVI
	priorityVGA
	priorityOther
)

var (
	monitorPriority = map[string]int{
		"edp":  priorityEDP,
		"dp":   priorityDP,
		"hdmi": priorityHDMI,
		"dvi":  priorityDVI,
		"vga":  priorityVGA,
	}
)

type touchscreenMapValue struct {
	OutputName string
	Auto       bool
}

//go:generate dbusutil-gen -output display_dbusutil.go -import github.com/godbus/dbus,github.com/linuxdeepin/go-x11-client -type Manager,Monitor manager.go monitor.go
type Manager struct {
	service                  *dbusutil.Service
	sysBus                   *dbus.Conn
	ofdbus                   *ofdbus.DBus
	inputDevices             *inputdevices.InputDevices
	xConn                    *x.Conn
	PropsMu                  sync.RWMutex
	config                   Config
	recommendScaleFactor     float64
	modes                    []randr.ModeInfo
	builtinMonitor           *Monitor
	builtinMonitorMu         sync.Mutex
	candidateBuiltinMonitors []*Monitor // 候补的
	monitorMap               map[randr.Output]*Monitor
	monitorMapMu             sync.Mutex
	crtcMap                  map[randr.Crtc]*randr.GetCrtcInfoReply
	crtcMapMu                sync.Mutex
	outputMap                map[randr.Output]*randr.GetOutputInfoReply
	outputMapMu              sync.Mutex
	configTimestamp          x.Timestamp
	settings                 *gio.Settings
	monitorsId               string
	isLaptop                 bool
	modeChanged              bool

	// dbusutil-gen: equal=nil
	Monitors []dbus.ObjectPath
	// dbusutil-gen: equal=nil
	CustomIdList []string
	HasChanged   bool
	DisplayMode  byte
	// dbusutil-gen: equal=nil
	Brightness map[string]float64
	// dbusutil-gen: equal=nil
	Touchscreens dxTouchscreens
	// dbusutil-gen: equal=nil
	TouchMap       map[string]string
	touchscreenMap map[string]touchscreenMapValue

	CurrentCustomId string
	Primary         string
	// dbusutil-gen: equal=nil
	PrimaryRect            x.Rectangle
	ScreenWidth            uint16
	ScreenHeight           uint16
	MaxBacklightBrightness uint32
	// method of adjust color temperature according to time and location
	ColorTemperatureMode gsprop.Enum `prop:"access:r"`
	// adjust color temperature by manual adjustment
	ColorTemperatureManual gsprop.Int `prop:"access:r"`

	methods *struct { //nolint
		AssociateTouch         func() `in:"outputName,touchSerial"`
		ChangeBrightness       func() `in:"raised"`
		DeleteCustomMode       func() `in:"name"`
		GetBrightness          func() `out:"values"`
		ListOutputNames        func() `out:"names"`
		ListOutputsCommonModes func() `out:"modes"`
		ModifyConfigName       func() `in:"name,newName"`
		SetAndSaveBrightness   func() `in:"outputName,value"`
		SetBrightness          func() `in:"outputName,value"`
		SetPrimary             func() `in:"outputName"`
		SwitchMode             func() `in:"mode,name"`
		CanRotate              func() `out:"can"`
		CanSetBrightness       func() `in:"outputName" out:"can"`
		GetBuiltinMonitor      func() `out:"name,path"`
		SetMethodAdjustCCT     func() `in:"adjustMethod"`
		SetColorTemperature    func() `in:"colorTemperature"`
		GetRealDisplayMode     func() `out:"mode"`
	}
}

type ModeInfo struct {
	Id     uint32
	name   string
	Width  uint16
	Height uint16
	Rate   float64
}

var _xConn *x.Conn

var _hasRandr1d2 bool // 是否 randr 版本大于等于 1.2

func Init(xConn *x.Conn) {
	_xConn = xConn
	randrVersion, err := randr.QueryVersion(xConn, randr.MajorVersion, randr.MinorVersion).Reply(xConn)
	if err != nil {
		logger.Warning(err)
	} else {
		logger.Debugf("randr version %d.%d", randrVersion.ServerMajorVersion, randrVersion.ServerMinorVersion)
		if randrVersion.ServerMajorVersion > 1 ||
			(randrVersion.ServerMajorVersion == 1 && randrVersion.ServerMinorVersion >= 2) {
			_hasRandr1d2 = true
		}
		logger.Debug("has randr1.2:", _hasRandr1d2)
	}
}

type monitorSizeInfo struct {
	width, height     uint16
	mmWidth, mmHeight uint32
}

func GetRecommendedScaleFactor() float64 {
	if !_hasRandr1d2 {
		return 1
	}
	resources, err := getScreenResources(_xConn)
	if err != nil {
		logger.Warning(err)
		return 1
	}
	cfgTs := resources.ConfigTimestamp

	var monitors []*monitorSizeInfo
	for _, output := range resources.Outputs {
		outputInfo, err := randr.GetOutputInfo(_xConn, output, cfgTs).Reply(_xConn)
		if err != nil {
			logger.Warningf("get output %v info failed: %v", output, err)
			return 1.0
		}
		if outputInfo.Connection != randr.ConnectionConnected {
			continue
		}

		crtcInfo, err := randr.GetCrtcInfo(_xConn, outputInfo.Crtc, cfgTs).Reply(_xConn)
		if err != nil {
			logger.Warningf("get crtc %v info failed: %v", outputInfo.Crtc, err)
			return 1.0
		}
		monitors = append(monitors, &monitorSizeInfo{
			mmWidth:  outputInfo.MmWidth,
			mmHeight: outputInfo.MmHeight,
			width:    crtcInfo.Width,
			height:   crtcInfo.Height,
		})
	}

	if len(monitors) == 0 {
		return 1.0
	}

	minScaleFactor := 3.0
	for _, monitor := range monitors {
		scaleFactor := calcRecommendedScaleFactor(float64(monitor.width), float64(monitor.height),
			float64(monitor.mmWidth), float64(monitor.mmHeight))
		if minScaleFactor > scaleFactor {
			minScaleFactor = scaleFactor
		}
	}
	return minScaleFactor
}

func newManager(service *dbusutil.Service) *Manager {
	m := &Manager{
		service:    service,
		monitorMap: make(map[randr.Output]*Monitor),
	}

	chassis, err := getComputeChassis()
	if err != nil {
		logger.Warning(err)
	}
	if chassis == "laptop" {
		m.isLaptop = true
	}

	m.settings = gio.NewSettings(gsSchemaDisplay)
	m.DisplayMode = uint8(m.settings.GetEnum(gsKeyDisplayMode))
	if m.DisplayMode == DisplayModeUnknow {
		m.DisplayMode = DisplayModeExtend
	}
	m.CurrentCustomId = m.settings.GetString(gsKeyCustomMode)
	m.ColorTemperatureManual.Bind(m.settings, gsKeyColorTemperatureManual)
	m.ColorTemperatureMode.Bind(m.settings, gsKeyColorTemperatureMode)
	m.xConn = _xConn

	screen := m.xConn.GetDefaultScreen()
	m.ScreenWidth = screen.WidthInPixels
	m.ScreenHeight = screen.HeightInPixels

	if _hasRandr1d2 {
		resources, err := getScreenResources(m.xConn)
		if err == nil {
			m.modes = resources.Modes
			m.configTimestamp = resources.ConfigTimestamp
			err = m.initCrtcMap(resources.Crtcs)
			if err != nil {
				logger.Warning(err)
			}
			err = m.initOutputMap(resources.Outputs)
			if err != nil {
				logger.Warning(err)
			}
		} else {
			logger.Warning(err)
		}

		for output, outputInfo := range m.outputMap {
			err = m.addMonitor(output, outputInfo)
			if err != nil {
				logger.Warning(err)
			}
		}

		m.initBuiltinMonitor()
		m.monitorsId = m.getMonitorsId()
		m.updatePropMonitors()
		m.updateOutputPrimary()

		m.config = loadConfig()
	} else {
		// randr 版本低于 1.2
		screenInfo, err := randr.GetScreenInfo(m.xConn, screen.Root).Reply(m.xConn)
		if err == nil {
			monitor, err := m.addMonitorFallback(screenInfo)
			if err == nil {
				m.updatePropMonitors()
				m.setPropPrimary("Default")
				m.setPropPrimaryRect(x.Rectangle{
					X:      monitor.X,
					Y:      monitor.Y,
					Width:  monitor.Width,
					Height: monitor.Height,
				})
			} else {
				logger.Warning(err)
			}
		} else {
			logger.Warning(err)
		}
	}

	m.CustomIdList = m.getCustomIdList()
	m.setPropMaxBacklightBrightness(uint32(brightness.GetMaxBacklightBrightness()))

	m.sysBus, err = dbus.SystemBus()
	if err != nil {
		logger.Warning(err)
	}

	sigLoop := dbusutil.NewSignalLoop(m.sysBus, 10)
	sigLoop.Start()

	m.ofdbus = ofdbus.NewDBus(m.sysBus)
	m.ofdbus.InitSignalExt(sigLoop, true)

	m.inputDevices = inputdevices.NewInputDevices(m.sysBus)
	m.inputDevices.InitSignalExt(sigLoop, true)

	return m
}

func (m *Manager) initBuiltinMonitor() {
	if !m.isLaptop {
		return
	}
	builtinMonitorName, err := loadBuiltinMonitorConfig(builtinMonitorConfigFile)
	if err != nil {
		logger.Warning(err)
	}
	monitors := m.getConnectedMonitors()
	if builtinMonitorName != "" {
		for _, monitor := range monitors {
			if monitor.Name == builtinMonitorName {
				m.builtinMonitor = monitor
			}
		}
	}

	if m.builtinMonitor != nil {
		return
	}
	builtinMonitorName = ""

	var rest []*Monitor
	for _, monitor := range monitors {
		name := strings.ToLower(monitor.Name)
		if strings.HasPrefix(name, "vga") {
			// ignore  VGA
		} else if strings.HasPrefix(name, "edp") {
			// 如果是 edp 开头，直接成为 builtinMonitor
			rest = []*Monitor{monitor}
			break
		} else {
			rest = append(rest, monitor)
		}
	}

	if len(rest) == 1 {
		m.builtinMonitor = rest[0]
		builtinMonitorName = m.builtinMonitor.Name
	} else if len(rest) > 1 {
		m.builtinMonitor = getMinIDMonitor(rest)
		// 但是不保存到配置文件中
		m.candidateBuiltinMonitors = rest
	}
	logger.Debug("rest:", rest)
	logger.Debug("m.builtinMonitor:", m.builtinMonitor)
	logger.Debug("m.candidateBuiltinMonitors:", m.candidateBuiltinMonitors)

	err = saveBuiltinMonitorConfig(builtinMonitorConfigFile, builtinMonitorName)
	if err != nil {
		logger.Warning("failed to save builtin monitor config:", err)
	}
}

func (m *Manager) updateBuiltinMonitorOnDisconnected(id uint32) {
	m.builtinMonitorMu.Lock()
	defer m.builtinMonitorMu.Unlock()

	if len(m.candidateBuiltinMonitors) < 2 {
		return
	}
	m.candidateBuiltinMonitors = monitorsRemove(m.candidateBuiltinMonitors, id)
	if len(m.candidateBuiltinMonitors) == 1 {
		// 当只剩下一个候补时能自动成为真的 builtin monitor
		m.builtinMonitor = m.candidateBuiltinMonitors[0]
		m.candidateBuiltinMonitors = nil
		err := saveBuiltinMonitorConfig(builtinMonitorConfigFile, m.builtinMonitor.Name)
		if err != nil {
			logger.Warning("failed to save builtin monitor config:", err)
		}
	}
}

func monitorsRemove(monitors []*Monitor, id uint32) []*Monitor {
	var result []*Monitor
	for _, m := range monitors {
		if m.ID != id {
			result = append(result, m)
		}
	}
	return result
}

func (m *Manager) applyDisplayMode() {
	// TODO 实现功能
	if !_hasRandr1d2 {
		return
	}
	logger.Debug("applyDisplayMode")
	monitors := m.getConnectedMonitors()
	var err error
	if len(monitors) == 1 {
		// 单屏
		screenCfg := m.getScreenConfig()
		var config *MonitorConfig
		if screenCfg.Single != nil {
			config = screenCfg.Single
		} else {
			config = monitors[0].toConfig()
			config.Enabled = true
			config.Primary = true
			mode := monitors[0].BestMode
			config.X = 0
			config.Y = 0
			config.Width = mode.Width
			config.Height = mode.Height
			config.RefreshRate = mode.Rate
			config.Rotation = randr.RotationRotate0
		}

		err = m.applyConfigs([]*MonitorConfig{config})
		if err != nil {
			logger.Warning("failed to apply configs:", err)
		}
		return
	}

	switch m.DisplayMode {
	case DisplayModeCustom:
		err = m.switchModeCustom(m.CurrentCustomId)
	case DisplayModeMirror:
		err = m.switchModeMirror()
	case DisplayModeExtend:
		err = m.switchModeExtend("")
	case DisplayModeOnlyOne:
		err = m.switchModeOnlyOne("")
	}

	if err != nil {
		logger.Warning(err)
	}
}

func (m *Manager) init() {
	brightness.InitBacklightHelper()
	m.initBrightness()
	m.applyDisplayMode()
	m.listenEvent() //等待applyDisplayMode执行完成再开启监听X事件
}

func (m *Manager) initColorTemperature() {
	method := m.ColorTemperatureMode.Get()
	err := m.SetMethodAdjustCCT(method)
	if err != nil {
		logger.Error(err)
	}
}

func calcRecommendedScaleFactor(widthPx, heightPx, widthMm, heightMm float64) float64 {
	if widthMm == 0 || heightMm == 0 {
		return 1
	}

	lenPx := math.Hypot(widthPx, heightPx)
	lenMm := math.Hypot(widthMm, heightMm)

	lenPxStd := math.Hypot(1920, 1080)
	lenMmStd := math.Hypot(477, 268)

	const a = 0.00158
	fix := (lenMm - lenMmStd) * (lenPx / lenPxStd) * a
	scaleFactor := (lenPx/lenMm)/(lenPxStd/lenMmStd) + fix

	return toListedScaleFactor(scaleFactor)
}

func toListedScaleFactor(s float64) float64 {
	const (
		min  = 1.0
		max  = 3.0
		step = 0.25
	)
	if s <= min {
		return min
	} else if s >= max {
		return max
	}

	for i := min; i <= max; i += step {
		if i > s {
			ii := i - step
			d1 := s - ii
			d2 := i - s

			if d1 >= d2 {
				return i
			} else {
				return ii
			}
		}
	}
	return max
}

func getScreenResources(xConn *x.Conn) (*randr.GetScreenResourcesReply, error) {
	root := xConn.GetDefaultScreen().Root
	resources, err := randr.GetScreenResources(xConn, root).Reply(xConn)
	return resources, err
}

func (m *Manager) getScreenResourcesCurrent() (*randr.GetScreenResourcesCurrentReply, error) {
	root := m.xConn.GetDefaultScreen().Root
	resources, err := randr.GetScreenResourcesCurrent(m.xConn, root).Reply(m.xConn)
	return resources, err
}

func (m *Manager) initCrtcMap(crtcs []randr.Crtc) error {
	m.crtcMap = make(map[randr.Crtc]*randr.GetCrtcInfoReply)
	for _, crtc := range crtcs {
		crtcInfo, err := m.getCrtcInfo(crtc)
		if err != nil {
			return err
		}
		m.crtcMap[crtc] = crtcInfo
	}
	return nil
}

func (m *Manager) initOutputMap(outputs []randr.Output) error {
	m.outputMap = make(map[randr.Output]*randr.GetOutputInfoReply)
	for _, output := range outputs {
		outputInfo, err := m.getOutputInfo(output)
		if err != nil {
			return err
		}
		m.outputMap[output] = outputInfo
	}
	return nil
}

func (m *Manager) getCrtcInfo(crtc randr.Crtc) (*randr.GetCrtcInfoReply, error) {
	m.PropsMu.RLock()
	cfgTs := m.configTimestamp
	m.PropsMu.RUnlock()

	crtcInfo, err := randr.GetCrtcInfo(m.xConn, crtc, cfgTs).Reply(m.xConn)
	if err != nil {
		return nil, err
	}
	if crtcInfo.Status != randr.StatusSuccess {
		return nil, fmt.Errorf("status is not success, is %v", crtcInfo.Status)
	}
	return crtcInfo, err
}

func (m *Manager) updateCrtcInfo(crtc randr.Crtc) (*randr.GetCrtcInfoReply, error) {
	crtcInfo, err := m.getCrtcInfo(crtc)
	if err != nil {
		return nil, err
	}
	m.crtcMapMu.Lock()
	m.crtcMap[crtc] = crtcInfo
	m.crtcMapMu.Unlock()
	return crtcInfo, nil
}

func (m *Manager) getOutputInfo(output randr.Output) (*randr.GetOutputInfoReply, error) {
	m.PropsMu.RLock()
	cfgTs := m.configTimestamp
	m.PropsMu.RUnlock()

	outputInfo, err := randr.GetOutputInfo(m.xConn, output, cfgTs).Reply(m.xConn)
	if err != nil {
		return nil, err
	}
	if outputInfo.Status != randr.StatusSuccess {
		return nil, fmt.Errorf("status is not success, is %v", outputInfo.Status)
	}
	return outputInfo, err
}

func (m *Manager) updateOutputInfo(output randr.Output) (*randr.GetOutputInfoReply, error) {
	outputInfo, err := m.getOutputInfo(output)
	if err != nil {
		return nil, err
	}
	m.outputMapMu.Lock()
	m.outputMap[output] = outputInfo
	m.outputMapMu.Unlock()
	return outputInfo, nil
}

func (m *Manager) getModeInfo(mode randr.Mode) ModeInfo {
	if mode == 0 {
		return ModeInfo{}
	}
	for _, modeInfo := range m.modes {
		if modeInfo.Id == uint32(mode) {
			return toModeInfo(modeInfo)
		}
	}
	return ModeInfo{}
}

func (m *Manager) getModeInfos(modes []randr.Mode) []ModeInfo {
	var result []ModeInfo
	for _, mode := range modes {
		modeInfo := m.getModeInfo(mode)
		if modeInfo.Id != 0 {
			result = append(result, modeInfo)
		}
	}
	result = filterModeInfosByRefreshRate(filterModeInfos(result))
	return result
}

func getScreenInfoSize(screenInfo *randr.GetScreenInfoReply) (size randr.ScreenSize, err error) {
	sizeId := screenInfo.SizeID
	if int(sizeId) < len(screenInfo.Sizes) {
		size = screenInfo.Sizes[sizeId]
	} else {
		err = fmt.Errorf("size id out of range: %d %d", sizeId, len(screenInfo.Sizes))
	}
	return
}

func (m *Manager) addMonitorFallback(screenInfo *randr.GetScreenInfoReply) (*Monitor, error) {
	output := randr.Output(1)

	size, err := getScreenInfoSize(screenInfo)
	if err != nil {
		return nil, err
	}

	monitor := &Monitor{
		service:   m.service,
		m:         m,
		ID:        uint32(output),
		Name:      "Default",
		Connected: true,
		MmWidth:   uint32(size.MWidth),
		MmHeight:  uint32(size.MHeight),
		Enabled:   true,
		Width:     size.Width,
		Height:    size.Height,
	}

	err = m.service.Export(monitor.getPath(), monitor)
	if err != nil {
		return nil, err
	}
	m.monitorMapMu.Lock()
	m.monitorMap[output] = monitor
	m.monitorMapMu.Unlock()
	return monitor, nil
}

func (m *Manager) updateMonitorFallback(screenInfo *randr.GetScreenInfoReply) *Monitor {
	output := randr.Output(1)
	m.monitorMapMu.Lock()
	monitor, ok := m.monitorMap[output]
	m.monitorMapMu.Unlock()
	if !ok {
		return nil
	}

	size, err := getScreenInfoSize(screenInfo)
	if err != nil {
		logger.Warning(err)
		return nil
	}

	monitor.setPropWidth(size.Width)
	monitor.setPropHeight(size.Height)
	monitor.setPropMmWidth(uint32(size.MWidth))
	monitor.setPropMmHeight(uint32(size.MHeight))
	return monitor
}

func (m *Manager) addMonitor(output randr.Output, outputInfo *randr.GetOutputInfoReply) error {
	m.monitorMapMu.Lock()
	_, ok := m.monitorMap[output]
	m.monitorMapMu.Unlock()
	if ok {
		return nil
	}

	var lastConnectedTime time.Time
	connected := outputInfo.Connection == randr.ConnectionConnected
	if connected {
		lastConnectedTime = time.Now()
	}
	enabled := outputInfo.Crtc != 0
	var err error
	var crtcInfo *randr.GetCrtcInfoReply
	if outputInfo.Crtc != 0 {
		m.crtcMapMu.Lock()
		crtcInfo = m.crtcMap[outputInfo.Crtc]
		m.crtcMapMu.Unlock()

		if crtcInfo == nil {
			crtcInfo, err = m.updateCrtcInfo(outputInfo.Crtc)
			if err != nil {
				logger.Warning(err)
			}
		}
	}

	edid, err := utils.GetOutputEDID(m.xConn, output)
	if err != nil {
		logger.Warning(err)
	}
	manufacturer, model := parseEDID(edid)
	logger.Debug("addMonitor", output, outputInfo.Name)
	monitor := &Monitor{
		service:           m.service,
		m:                 m,
		ID:                uint32(output),
		Name:              outputInfo.Name,
		Connected:         connected,
		MmWidth:           outputInfo.MmWidth,
		MmHeight:          outputInfo.MmHeight,
		Enabled:           enabled,
		crtc:              outputInfo.Crtc,
		uuid:              getOutputUUID(outputInfo.Name, edid),
		Manufacturer:      manufacturer,
		Model:             model,
		lastConnectedTime: lastConnectedTime,
	}

	monitor.Modes = m.getModeInfos(outputInfo.Modes)
	monitor.BestMode = monitor.getBestMode(m, outputInfo)
	monitor.PreferredModes = []ModeInfo{monitor.BestMode}

	if crtcInfo != nil {
		monitor.X = crtcInfo.X
		monitor.Y = crtcInfo.Y
		monitor.Width = crtcInfo.Width
		monitor.Height = crtcInfo.Height

		monitor.Reflects = getReflects(crtcInfo.Rotations)
		monitor.Rotations = getRotations(crtcInfo.Rotations)
		monitor.Rotation, monitor.Reflect = parseCrtcRotation(crtcInfo.Rotation)

		monitor.CurrentMode = m.getModeInfo(crtcInfo.Mode)
		monitor.RefreshRate = monitor.CurrentMode.Rate
	}
	monitor.oldRotation = monitor.Rotation

	err = m.service.Export(monitor.getPath(), monitor)
	if err != nil {
		return err
	}
	m.monitorMapMu.Lock()
	m.monitorMap[output] = monitor
	m.monitorMapMu.Unlock()
	return nil
}

func (m *Manager) updateMonitor(output randr.Output, outputInfo *randr.GetOutputInfoReply) {
	m.monitorMapMu.Lock()
	monitor, ok := m.monitorMap[output]
	m.monitorMapMu.Unlock()
	if !ok {
		err := m.addMonitor(output, outputInfo)
		if err != nil {
			logger.Warning(err)
			return
		}

		return
	}

	connected := outputInfo.Connection == randr.ConnectionConnected
	enabled := outputInfo.Crtc != 0
	var err error
	var crtcInfo *randr.GetCrtcInfoReply
	if outputInfo.Crtc != 0 {
		m.crtcMapMu.Lock()
		crtcInfo = m.crtcMap[outputInfo.Crtc]
		m.crtcMapMu.Unlock()

		if crtcInfo == nil {
			crtcInfo, err = m.updateCrtcInfo(outputInfo.Crtc)
			if err != nil {
				logger.Warning(err)
			}
		}
	}

	var edid []byte
	var lastConnectedTime time.Time
	if connected {
		edid, err = utils.GetOutputEDID(m.xConn, output)
		if err != nil {
			logger.Warning(err)
		}
		lastConnectedTime = time.Now()
	} else {
		m.updateBuiltinMonitorOnDisconnected(monitor.ID)
	}
	manufacturer, model := parseEDID(edid)
	uuid := getOutputUUID(outputInfo.Name, edid)
	monitor.PropsMu.Lock()
	monitor.uuid = uuid
	monitor.crtc = outputInfo.Crtc
	monitor.lastConnectedTime = lastConnectedTime
	monitor.setPropManufacturer(manufacturer)
	monitor.setPropModel(model)
	monitor.setPropConnected(connected)
	monitor.setPropEnabled(enabled)
	monitor.setPropModes(m.getModeInfos(outputInfo.Modes))
	monitor.setPropBestMode(monitor.getBestMode(m, outputInfo))
	monitor.setPropPreferredModes([]ModeInfo{monitor.BestMode})
	monitor.setPropMmWidth(outputInfo.MmWidth)
	monitor.setPropMmHeight(outputInfo.MmHeight)
	monitor.PropsMu.Unlock()
	m.updateMonitorCrtcInfo(monitor, crtcInfo)
}

func (m *Manager) updateMonitorCrtcInfo(monitor *Monitor, crtcInfo *randr.GetCrtcInfoReply) {
	if crtcInfo == nil {
		monitor.PropsMu.Lock()
		monitor.setPropX(0)
		monitor.setPropY(0)
		monitor.setPropWidth(0)
		monitor.setPropHeight(0)

		monitor.setPropReflects(nil)
		monitor.setPropRotations(nil)
		monitor.setPropRotation(0)
		monitor.setPropReflect(0)

		monitor.setPropCurrentMode(ModeInfo{})
		monitor.setPropRefreshRate(0)
		monitor.PropsMu.Unlock()
		return
	}

	rotation, reflect := parseCrtcRotation(crtcInfo.Rotation)
	monitor.PropsMu.Lock()
	monitor.setPropX(crtcInfo.X)
	monitor.setPropY(crtcInfo.Y)
	monitor.setPropWidth(crtcInfo.Width)
	monitor.setPropHeight(crtcInfo.Height)

	monitor.setPropReflects(getReflects(crtcInfo.Rotations))
	monitor.setPropRotations(getRotations(crtcInfo.Rotations))
	monitor.setPropRotation(rotation)
	monitor.setPropReflect(reflect)

	monitor.setPropCurrentMode(m.getModeInfo(crtcInfo.Mode))
	monitor.setPropRefreshRate(monitor.CurrentMode.Rate)
	monitor.PropsMu.Unlock()
}

func (m *Manager) findFreeCrtc(output randr.Output) randr.Crtc {
	m.crtcMapMu.Lock()
	defer m.crtcMapMu.Unlock()

	for crtc, crtcInfo := range m.crtcMap {
		if len(crtcInfo.Outputs) == 0 && outputSliceContains(crtcInfo.PossibleOutputs, output) {
			return crtc
		}
	}
	return 0
}

func (m *Manager) switchModeMirrorAux() (err error, monitor0 *Monitor) {
	logger.Debug("switch mode mirror")
	screenCfg := m.getScreenConfig()
	configs := screenCfg.getMonitorConfigs(DisplayModeMirror, "")
	monitors := m.getConnectedMonitors()
	commonSizes := getMonitorsCommonSizes(monitors)
	if len(commonSizes) == 0 {
		err = errors.New("not found common size")
		return
	}
	maxSize := getMaxAreaSize(commonSizes)
	logger.Debug("max common size:", maxSize)
	for _, monitor := range m.monitorMap {
		if monitor.Connected {
			monitor.enable(true)

			cfg := getMonitorConfigByUuid(configs, monitor.uuid)
			var mode ModeInfo
			if cfg != nil {
				mode = monitor.selectMode(cfg.Width, cfg.Height, cfg.RefreshRate)
			} else {
				mode = getFirstModeBySize(monitor.Modes, maxSize.width, maxSize.height)
			}
			monitor.setMode(mode)
			monitor.setPosition(0, 0)
			monitor.setRotation(randr.RotationRotate0)
			monitor.setReflect(0)

		} else {
			monitor.enable(false)
		}
	}

	err = m.apply()
	if err != nil {
		return
	}

	monitor0 = m.getDefaultPrimaryMonitor(m.getConnectedMonitors())
	if monitor0 != nil {
		err = m.setOutputPrimary(randr.Output(monitor0.ID))
		if err != nil {
			return
		}
	}
	return
}

func (m *Manager) switchModeMirror() (err error) {
	err, monitor0 := m.switchModeMirrorAux()
	if err != nil {
		return
	}

	screenCfg := m.getScreenConfig()
	screenCfg.setMonitorConfigs(DisplayModeMirror, "",
		toMonitorConfigs(m.getConnectedMonitors(), monitor0.Name))
	return m.saveConfig()
}

type screenSize struct {
	width    uint16
	height   uint16
	mmWidth  uint32
	mmHeight uint32
}

func (m *Manager) getScreenSize1() screenSize {
	width, height := m.getScreenSize()
	mmWidth := uint32(float64(width) / 3.792)
	mmHeight := uint32(float64(height) / 3.792)
	return screenSize{
		width:    width,
		height:   height,
		mmWidth:  mmWidth,
		mmHeight: mmHeight,
	}
}

func (m *Manager) setScreenSize(ss screenSize) error {
	root := m.xConn.GetDefaultScreen().Root
	err := randr.SetScreenSizeChecked(m.xConn, root, ss.width, ss.height, ss.mmWidth,
		ss.mmHeight).Check(m.xConn)
	logger.Debugf("set screen size %dx%d, mm: %dx%d",
		ss.width, ss.height, ss.mmWidth, ss.mmHeight)
	return err
}

type crtcConfig struct {
	crtc    randr.Crtc
	outputs []randr.Output

	x        int16
	y        int16
	rotation uint16
	mode     randr.Mode
}

func (m *Manager) apply() error {
	x.GrabServer(m.xConn)
	defer func() {
		err := x.UngrabServerChecked(m.xConn).Check(m.xConn)
		if err != nil {
			logger.Warning(err)
		}
	}()

	monitorCrtcCfgMap := make(map[randr.Output]crtcConfig)
	for output, monitor := range m.monitorMap {
		if monitor.Enabled {
			crtc := monitor.crtc
			if crtc == 0 {
				crtc = m.findFreeCrtc(output)
				if crtc == 0 {
					return errors.New("failed to find free crtc")
				}
			}
			monitorCrtcCfgMap[output] = crtcConfig{
				crtc:     crtc,
				x:        monitor.X,
				y:        monitor.Y,
				mode:     randr.Mode(monitor.CurrentMode.Id),
				rotation: monitor.Rotation | monitor.Reflect,
				outputs:  []randr.Output{output},
			}
		} else {
			if monitor.crtc != 0 {
				monitorCrtcCfgMap[output] = crtcConfig{
					crtc:     monitor.crtc,
					rotation: randr.RotationRotate0,
				}
			}
		}
	}

	m.PropsMu.RLock()
	cfgTs := m.configTimestamp
	m.PropsMu.RUnlock()

	screenSize := m.getScreenSize1()

	m.crtcMapMu.Lock()
	for crtc, crtcInfo := range m.crtcMap {
		rect := getCrtcRect(crtcInfo)
		logger.Debugf("crtc %v, rect: %+v", crtc, rect)
		if int(rect.X)+int(rect.Width) <= int(screenSize.width) &&
			int(rect.Y)+int(rect.Height) <= int(screenSize.height) {
			// 适合
			monitors := m.getConnectedMonitors()
			for _, monitor := range monitors {
				if monitor.crtc == crtc {
					if monitor.oldRotation != monitor.Rotation || m.modeChanged {
						monitor.oldRotation = monitor.Rotation
						logger.Debugf("disable crtc %v, it's outputs: %v", crtc, crtcInfo.Outputs)
						err := m.disableCrtc(crtc, cfgTs)
						if err != nil {
							return err
						}
					}
				}
			}

		} else {
			// 不适合新的屏幕大小，如果已经启用，则需要禁用它
			if len(crtcInfo.Outputs) == 0 {
				continue
			}
			logger.Debugf("disable crtc %v, it's outputs: %v", crtc, crtcInfo.Outputs)
			err := m.disableCrtc(crtc, cfgTs)
			if err != nil {
				return err
			}
		}
	}
	m.modeChanged = false
	m.crtcMapMu.Unlock()

	err := m.setScreenSize(screenSize)
	if err != nil {
		return err
	}

	for output, monitor := range m.monitorMap {
		crtcCfg, ok := monitorCrtcCfgMap[output]
		if !ok {
			continue
		}
		err := monitor.applyConfig(crtcCfg)
		if err != nil {
			return err
		}

		outputInfo, err := m.updateOutputInfo(output)
		if err != nil {
			logger.Warning(err)
		}
		if outputInfo.Crtc != 0 {
			_, err = m.updateCrtcInfo(outputInfo.Crtc)
			if err != nil {
				logger.Warning(err)
			}
		}
		m.updateMonitor(output, outputInfo)

		if monitor.Enabled {
			m.PropsMu.Lock()
			value, ok := m.Brightness[monitor.Name]
			if !ok {
				value = 1
				m.Brightness[monitor.Name] = value
				_ = m.emitPropChangedBrightness(m.Brightness)
			}
			m.PropsMu.Unlock()

			go func(mon *Monitor) {
				err = m.setMonitorBrightness(mon, value)
				if err != nil {
					logger.Warningf("failed to set brightness for %s: %v", mon.Name, err)
				}
			}(monitor) // 用局部变量作闭包上值
		}
	}

	return nil
}

func (m *Manager) disableCrtc(crtc randr.Crtc, cfgTs x.Timestamp) error {
	setCfg, err := randr.SetCrtcConfig(m.xConn, crtc, 0, cfgTs,
		0, 0, 0, randr.RotationRotate0, nil).Reply(m.xConn)
	if err != nil {
		return err
	}
	if setCfg.Status != randr.SetConfigSuccess {
		return fmt.Errorf("failed to disable crtc %d: %v",
			crtc, getRandrStatusStr(setCfg.Status))
	}
	return nil
}

func (m *Manager) setOutputPrimary(output randr.Output) error {
	logger.Debug("set output primary", output)
	root := m.xConn.GetDefaultScreen().Root
	return randr.SetOutputPrimaryChecked(m.xConn, root, output).Check(m.xConn)
}

func (m *Manager) getOutputPrimary() (randr.Output, error) {
	root := m.xConn.GetDefaultScreen().Root
	reply, err := randr.GetOutputPrimary(m.xConn, root).Reply(m.xConn)
	if err != nil {
		return 0, err
	}
	return reply.Output, nil
}

// 更新属性 Primary 和 PrimaryRect
func (m *Manager) updateOutputPrimary() {
	pOutput, err := m.getOutputPrimary()
	if err != nil {
		logger.Warning(err)
		return
	}

	var newPrimary string
	var newRect x.Rectangle

	if pOutput != 0 {
		m.outputMapMu.Lock()

		for output, outputInfo := range m.outputMap {
			if pOutput != output {
				continue
			}

			newPrimary = outputInfo.Name

			if outputInfo.Crtc == 0 {
				logger.Warning("new primary output crtc is 0")
			} else {
				m.crtcMapMu.Lock()
				crtcInfo := m.crtcMap[outputInfo.Crtc]
				m.crtcMapMu.Unlock()
				if crtcInfo == nil {
					logger.Warning("crtcInfo is nil")
				} else {
					newRect = getCrtcRect(crtcInfo)
				}
			}
			break
		}

		m.outputMapMu.Unlock()
	}

	m.PropsMu.Lock()
	m.setPropPrimary(newPrimary)
	m.setPropPrimaryRect(newRect)
	m.PropsMu.Unlock()

	logger.Debugf("updateOutputPrimary name: %q, rect: %#v", newPrimary, newRect)
}

func (m *Manager) setPrimary(name string) error {
	switch m.DisplayMode {
	case DisplayModeMirror:
		return errors.New("not allow set primary in mirror mode")

	case DisplayModeOnlyOne:
		return m.switchModeOnlyOne(name)

	case DisplayModeExtend, DisplayModeCustom:
		screenCfg := m.getScreenConfig()
		configs := screenCfg.getMonitorConfigs(m.DisplayMode, m.CurrentCustomId)

		var monitor0 *Monitor
		for _, monitor := range m.monitorMap {
			if monitor.Name != name {
				continue
			}

			if !monitor.Connected {
				return errors.New("monitor is not connected")
			}

			monitor0 = monitor
			break
		}

		if monitor0 == nil {
			return errors.New("not found monitor")
		}

		if len(configs) == 0 {
			if m.DisplayMode == DisplayModeCustom {
				return errors.New("custom mode configs is empty")
			}
			configs = toMonitorConfigs(m.getConnectedMonitors(), monitor0.Name)
		} else {
			// modify configs
			updateMonitorConfigsName(configs, m.monitorMap)
			setMonitorConfigsPrimary(configs, monitor0.uuid)
		}

		err := m.setOutputPrimary(randr.Output(monitor0.ID))
		if err != nil {
			return err
		}

		screenCfg.setMonitorConfigs(m.DisplayMode, m.CurrentCustomId, configs)

		err = m.saveConfig()
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid display mode %v", m.DisplayMode)
	}
	return nil
}

func (m *Manager) switchModeExtend(primary string) (err error) {
	logger.Debug("switch mode extend")
	var monitors []*Monitor
	for _, monitor := range m.monitorMap {
		monitors = append(monitors, monitor)
	}
	sortMonitorsByID(monitors)
	screenCfg := m.getScreenConfig()
	configs := screenCfg.getMonitorConfigs(DisplayModeExtend, "")

	var xOffset int
	var monitor0 *Monitor
	for _, monitor := range monitors {
		if monitor.Connected {
			monitor.enable(true)

			cfg := getMonitorConfigByUuid(configs, monitor.uuid)
			var mode ModeInfo
			if cfg != nil {
				mode = monitor.selectMode(cfg.Width, cfg.Height, cfg.RefreshRate)
				if monitor0 == nil && cfg.Primary {
					monitor0 = monitor
				}

			} else {
				mode = monitor.BestMode
			}

			monitor.setMode(mode)

			if xOffset > math.MaxInt16 {
				xOffset = math.MaxInt16
			}
			monitor.setPosition(int16(xOffset), 0)
			monitor.setRotation(randr.RotationRotate0)
			monitor.setReflect(0)

			xOffset += int(monitor.Width)
		} else {
			monitor.enable(false)
		}
	}

	if monitor0 == nil {
		monitor0 = m.getDefaultPrimaryMonitor(m.getConnectedMonitors())
	}

	err = m.apply()
	if err != nil {
		return
	}

	if monitor0 != nil {
		err = m.setOutputPrimary(randr.Output(monitor0.ID))
		if err != nil {
			logger.Warning("failed to set primary output:", err)
			return
		}

		screenCfg.setMonitorConfigs(DisplayModeExtend, "",
			toMonitorConfigs(m.getConnectedMonitors(), monitor0.Name))

		err = m.saveConfig()
		if err != nil {
			logger.Warning("failed to save config:", err)
			return
		}
	}

	return
}

func (m *Manager) getScreenConfig() *ScreenConfig {
	id := m.getMonitorsId()
	screenCfg := m.config[id]
	if screenCfg == nil {
		screenCfg = &ScreenConfig{}
		m.config[id] = screenCfg
	}
	return screenCfg
}

func (m *Manager) switchModeOnlyOne(name string) (err error) {
	logger.Debug("switch mode only one", name)

	screenCfg := m.getScreenConfig()
	configs := screenCfg.getMonitorConfigs(DisplayModeOnlyOne, "")

	var monitor0 *Monitor
	var needSaveCfg bool
	if name != "" {
		needSaveCfg = true
		for _, monitor := range m.monitorMap {
			if monitor.Name == name {
				monitor0 = monitor

				if !monitor.Connected {
					err = errors.New("monitor is not connected")
					return
				}

				break
			}
		}
		if monitor0 == nil {
			err = errors.New("not found monitor")
			return
		}
	} else {
		var enableUuid string
		for _, cfg := range configs {
			if cfg.Enabled {
				enableUuid = cfg.UUID
				break
			}
		}
		if enableUuid != "" {
			for _, monitor := range m.monitorMap {
				if monitor.uuid == enableUuid {
					monitor0 = monitor
					break
				}
			}
		}

		if monitor0 == nil {
			needSaveCfg = true
			monitor0 = getMinIDMonitor(m.getConnectedMonitors())
		}

	}
	if monitor0 == nil {
		err = errors.New("monitor0 is nil")
		return
	}

	for _, monitor := range m.monitorMap {
		if monitor.uuid == monitor0.uuid {
			monitor.enable(true)
			cfg := getMonitorConfigByUuid(configs, monitor.uuid)
			var mode ModeInfo
			var rotation uint16 = randr.RotationRotate0
			var reflect uint16
			if cfg != nil {
				mode = monitor.selectMode(cfg.Width, cfg.Height, cfg.RefreshRate)
				rotation = cfg.Rotation
				reflect = cfg.Reflect
			} else {
				mode = monitor.BestMode
			}

			monitor.setMode(mode)
			monitor.setPosition(0, 0)
			monitor.setRotation(rotation)
			monitor.setReflect(reflect)

		} else {
			monitor.enable(false)
		}
	}

	err = m.apply()
	if err != nil {
		return
	}

	// set primary output
	err = m.setOutputPrimary(randr.Output(monitor0.ID))
	if err != nil {
		return
	}

	if needSaveCfg {
		screenCfg.setMonitorConfigs(DisplayModeOnlyOne, "",
			toMonitorConfigs(m.getConnectedMonitors(), monitor0.Name))

		err = m.saveConfig()
		if err != nil {
			return
		}
	}

	return
}

func (m *Manager) switchModeCustom(name string) (err error) {
	logger.Debug("switch mode custom", name)
	if name == "" {
		err = errors.New("name is empty")
		return
	}

	screenCfg := m.getScreenConfig()
	configs := screenCfg.getMonitorConfigs(DisplayModeCustom, name)
	if len(configs) > 0 {
		err = m.applyConfigs(configs)
		return
	}

	// 自定义配置不存在时，默认使用复制模式，即自定义模式的合并子模式
	err, monitor0 := m.switchModeMirrorAux()
	if err != nil {
		return
	}

	screenCfg.setMonitorConfigs(DisplayModeCustom, name,
		toMonitorConfigs(m.getConnectedMonitors(), monitor0.Name))
	err = m.saveConfig()
	if err != nil {
		return
	}
	m.setPropCustomIdList(m.getCustomIdList())
	return
}

func (m *Manager) switchMode(mode byte, name string) (err error) {
	switch mode {
	case DisplayModeMirror:
		err = m.switchModeMirror()
	case DisplayModeExtend:
		err = m.switchModeExtend("")
	case DisplayModeOnlyOne:
		err = m.switchModeOnlyOne(name)
	case DisplayModeCustom:
		err = m.switchModeCustom(name)
		if err == nil {
			m.setCurrentCustomId(name)
		}
	default:
		err = errors.New("invalid mode")
	}
	if err == nil {
		m.setDisplayMode(mode)
		m.modeChanged = true
	} else {
		logger.Warningf("failed to switch mode %v %v: %v", mode, name, err)
	}
	return
}

func (m *Manager) setDisplayMode(mode byte) {
	m.setPropDisplayMode(mode)
	m.settings.SetEnum(gsKeyDisplayMode, int32(mode))
}

func (m *Manager) save() (err error) {
	logger.Debug("save")
	id := m.getMonitorsId()
	if id == "" {
		err = errors.New("no output connected")
		return
	}

	screenCfg := m.config[id]
	if screenCfg == nil {
		screenCfg = &ScreenConfig{}
		m.config[id] = screenCfg
	}
	monitors := m.getConnectedMonitors()

	if len(monitors) == 1 {
		screenCfg.Single = monitors[0].toConfig()
	} else {
		screenCfg.setMonitorConfigs(m.DisplayMode, m.CurrentCustomId,
			toMonitorConfigs(monitors, m.Primary))
	}

	err = m.saveConfig()
	if err != nil {
		return err
	}
	m.markClean()
	return nil
}

func (m *Manager) markClean() {
	m.monitorMapMu.Lock()
	for _, monitor := range m.monitorMap {
		monitor.backup = nil
	}
	m.monitorMapMu.Unlock()

	m.PropsMu.Lock()
	m.setPropHasChanged(false)
	m.PropsMu.Unlock()
}

func (m *Manager) getConnectedMonitors() Monitors {
	m.monitorMapMu.Lock()
	var monitors Monitors
	for _, monitor := range m.monitorMap {
		if monitor.Connected {
			monitors = append(monitors, monitor)
		}
	}
	m.monitorMapMu.Unlock()
	return monitors
}

func (m *Manager) setCurrentCustomId(name string) {
	m.setPropCurrentCustomId(name)
	m.settings.SetString(gsKeyCustomMode, name)
}

func (m *Manager) applyConfigs(configs []*MonitorConfig) error {
	logger.Debug("applyConfigs", spew.Sdump(configs))
	var primaryOutput randr.Output
	for output, monitor := range m.monitorMap {
		monitorCfg := getMonitorConfigByUuid(configs, monitor.uuid)
		if monitorCfg == nil {
			monitor.enable(false)
		} else {
			if monitorCfg.Primary && monitorCfg.Enabled {
				primaryOutput = output
			}
			monitor.enable(monitorCfg.Enabled)
			monitor.setPosition(monitorCfg.X, monitorCfg.Y)
			monitor.setRotation(monitorCfg.Rotation)
			monitor.setReflect(monitorCfg.Reflect)

			width := monitorCfg.Width
			height := monitorCfg.Height
			if needSwapWidthHeight(monitorCfg.Rotation) {
				width, height = height, width
			}
			mode := monitor.selectMode(width, height, monitorCfg.RefreshRate)
			monitor.setMode(mode)
		}
	}
	err := m.apply()
	if err != nil {
		return err
	}
	if primaryOutput == 0 {
		primaryOutput = randr.Output(m.getDefaultPrimaryMonitor(m.getConnectedMonitors()).ID)
	}
	err = m.setOutputPrimary(primaryOutput)
	if err != nil {
		return err
	}
	return nil
}

func (m *Manager) getDefaultPrimaryMonitor(monitors []*Monitor) *Monitor {
	if len(monitors) == 0 {
		return nil
	}
	builtinMonitor := m.getBuiltinMonitor()
	if builtinMonitor != nil {
		return builtinMonitor
	}

	monitor := m.getPriorMonitor(monitors)
	if monitor != nil {
		return monitor
	}

	return getMinLastConnectedTimeMonitor(monitors)
}

func (m *Manager) getPriorMonitor(monitors []*Monitor) *Monitor {
	var monitor *Monitor
	priority := priorityOther
	for _, v := range monitors {
		name := m.getPortType(v.Name)
		p, ok := monitorPriority[name]

		// 不在列表中的话，留空，以便最后通过连接时间来设置主屏幕
		if !ok {
			continue
		}

		if p < priority {
			monitor = v
			priority = p
			continue
		}

		// 当接口类型相同，若一个是默认显示器，继续让它作为默认显示器
		if p == priority && m.Primary == v.Name {
			monitor = v
			priority = p
		}
	}

	return monitor
}

func (m *Manager) getPortType(name string) string {
	i := strings.IndexRune(name, '-')
	if i != -1 {
		name = name[0 : i+1]
	}
	return strings.ToLower(name)
}

func (m *Manager) getCustomIdList() []string {
	id := m.getMonitorsId()

	screenCfg := m.config[id]
	if screenCfg == nil {
		return nil
	}

	result := make([]string, len(screenCfg.Custom))
	for idx, custom := range screenCfg.Custom {
		result[idx] = custom.Name
	}
	sort.Strings(result)
	return result
}

func (m *Manager) getMonitorsId() string {
	var ids []string
	m.monitorMapMu.Lock()
	for _, monitor := range m.monitorMap {
		if !monitor.Connected {
			continue
		}
		ids = append(ids, monitor.uuid)
	}
	m.monitorMapMu.Unlock()
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return strings.Join(ids, monitorsIdDelimiter)
}

func (m *Manager) updatePropMonitors() {
	monitors := m.getConnectedMonitors()
	sort.Slice(monitors, func(i, j int) bool {
		return monitors[i].ID < monitors[j].ID
	})
	paths := make([]dbus.ObjectPath, len(monitors))
	for i, monitor := range monitors {
		paths[i] = monitor.getPath()
	}
	m.setPropMonitors(paths)
}

func (m *Manager) modifyConfigName(name, newName string) (err error) {
	if name == "" || newName == "" {
		err = errors.New("name is empty")
		return
	}

	id := m.getMonitorsId()
	if id == "" {
		err = errors.New("no output connected")
		return
	}

	screenCfg := m.config[id]
	if screenCfg == nil {
		err = errors.New("not found screen config")
		return
	}

	var customConfig *CustomModeConfig
	for _, custom := range screenCfg.Custom {
		if custom.Name == name {
			customConfig = custom
			break
		}
	}
	if customConfig == nil {
		err = fmt.Errorf("not found custom mode config %q", name)
		return
	}
	if name == newName {
		return nil
	}

	for _, custom := range screenCfg.Custom {
		if custom.Name == newName {
			err = fmt.Errorf("same name config %q already exists", newName)
			return
		}
	}

	customConfig.Name = newName
	m.setPropCustomIdList(m.getCustomIdList())
	if name == m.CurrentCustomId {
		m.setCurrentCustomId(newName)
	}

	err = m.saveConfig()
	if err != nil {
		return err
	}

	return nil
}

func (m *Manager) deleteCustomMode(name string) (err error) {
	logger.Debugf("deleteCustomMode %q", name)
	if name == "" {
		err = errors.New("name is empty")
		return
	}

	id := m.getMonitorsId()
	if id == "" {
		err = errors.New("no output connected")
		return
	}

	if m.isCustomModeBeingUsed(name) {
		err = errors.New("the custom mode is being used")
		return
	}

	screenCfg := m.config[id]
	if screenCfg == nil {
		err = errors.New("not found screen config")
		return
	}

	var customConfigs []*CustomModeConfig
	foundName := false
	for _, custom := range screenCfg.Custom {
		if custom.Name == name {
			foundName = true
		} else {
			customConfigs = append(customConfigs, custom)
		}
	}

	if !foundName {
		logger.Warning("not found custom mode config:", name)
		// not found
		return nil
	}

	screenCfg.Custom = customConfigs

	if m.CurrentCustomId == name {
		m.setCurrentCustomId("")
	}

	m.setPropCustomIdList(m.getCustomIdList())
	err = m.saveConfig()
	if err != nil {
		return err
	}

	return nil
}

func (m *Manager) isCustomModeBeingUsed(name string) bool {
	return m.DisplayMode == DisplayModeCustom &&
		m.CurrentCustomId == name
}

func (m *Manager) getScreenSize() (sw, sh uint16) {
	var w, h int
	for _, monitor := range m.monitorMap {
		if !monitor.Connected || !monitor.Enabled {
			continue
		}

		width := monitor.CurrentMode.Width
		height := monitor.CurrentMode.Height

		if needSwapWidthHeight(monitor.Rotation) {
			width, height = height, width
		}

		w1 := int(monitor.X) + int(width)
		h1 := int(monitor.Y) + int(height)

		if w < w1 {
			w = w1
		}
		if h < h1 {
			h = h1
		}
	}
	if w > math.MaxUint16 {
		w = math.MaxUint16
	}
	if h > math.MaxUint16 {
		h = math.MaxUint16
	}
	sw = uint16(w)
	sh = uint16(h)
	return
}

func (m *Manager) newTouchscreen(path dbus.ObjectPath) (*Touchscreen, error) {
	t, err := inputdevices.NewTouchscreen(m.sysBus, path)
	if err != nil {
		return nil, err
	}

	touchscreen := &Touchscreen{
		path: path,
	}
	touchscreen.Name, _ = t.Name().Get(0)
	touchscreen.DeviceNode, _ = t.DevNode().Get(0)
	touchscreen.Serial, _ = t.Serial().Get(0)
	touchscreen.uuid, _ = t.UUID().Get(0)
	touchscreen.outputName, _ = t.OutputName().Get(0)
	touchscreen.width, _ = t.Width().Get(0)
	touchscreen.height, _ = t.Height().Get(0)

	touchscreen.busType = BusTypeUnknown
	busType, _ := t.BusType().Get(0)
	if strings.ToLower(busType) == "usb" {
		touchscreen.busType = BusTypeUSB
	}

	getXTouchscreenInfo(touchscreen)
	if touchscreen.Id == 0 {
		return nil, xerrors.New("no mathced touchscreen ID")
	}

	return touchscreen, nil
}

func (m *Manager) removeTouchscreenByIdx(i int) {
	// see https://github.com/golang/go/wiki/SliceTricks
	m.Touchscreens[i] = m.Touchscreens[len(m.Touchscreens)-1]
	m.Touchscreens[len(m.Touchscreens)-1] = nil
	m.Touchscreens = m.Touchscreens[:len(m.Touchscreens)-1]
}

func (m *Manager) removeTouchscreenByPath(path dbus.ObjectPath) {
	i := -1
	for index, v := range m.Touchscreens {
		if v.path == path {
			i = index
		}
	}

	if i == -1 {
		return
	}

	m.removeTouchscreenByIdx(i)
}

func (m *Manager) removeTouchscreenByDeviceNode(deviceNode string) {
	i := -1
	for idx, v := range m.Touchscreens {
		if v.DeviceNode == deviceNode {
			i = idx
			break
		}
	}

	if i == -1 {
		return
	}

	m.removeTouchscreenByIdx(i)
}

func (m *Manager) initTouchscreens() {
	m.ofdbus.ConnectNameOwnerChanged(func(name, oldOwner, newOwner string) {
		if name == m.inputDevices.ServiceName_() && newOwner == "" {
			m.setPropTouchscreens(nil)
		}
	})

	_, err := m.inputDevices.ConnectTouchscreenAdded(func(path dbus.ObjectPath) {
		getDeviceInfos(true)

		// 通过 path 删除重复设备
		m.removeTouchscreenByPath(path)

		touchscreen, err := m.newTouchscreen(path)
		if err != nil {
			logger.Warning(err)
			return
		}

		// 若设备已存在，删除并重新添加
		m.removeTouchscreenByDeviceNode(touchscreen.DeviceNode)

		m.Touchscreens = append(m.Touchscreens, touchscreen)
		m.emitPropChangedTouchscreens(m.Touchscreens)

		m.handleTouchscreenChanged()
	})
	if err != nil {
		logger.Warning(err)
	}

	_, err = m.inputDevices.ConnectTouchscreenRemoved(func(path dbus.ObjectPath) {
		m.removeTouchscreenByPath(path)
		m.emitPropChangedTouchscreens(m.Touchscreens)
	})
	if err != nil {
		logger.Warning(err)
	}

	touchscreens, err := m.inputDevices.Touchscreens().Get(0)
	if err != nil {
		logger.Warning(err)
		return
	}

	getDeviceInfos(true)
	for _, p := range touchscreens {
		touchscreen, err := m.newTouchscreen(p)
		if err != nil {
			logger.Warning(err)
			continue
		}

		m.Touchscreens = append(m.Touchscreens, touchscreen)
	}
	m.emitPropChangedTouchscreens(m.Touchscreens)

	m.initTouchMap()
	m.handleTouchscreenChanged()
}

func (m *Manager) initTouchMap() {
	m.touchscreenMap = make(map[string]touchscreenMapValue)
	m.TouchMap = make(map[string]string)

	value := m.settings.GetString(gsKeyMapOutput)
	if len(value) == 0 {
		return
	}

	err := jsonUnmarshal(value, &m.touchscreenMap)
	if err != nil {
		logger.Warningf("[initTouchMap] unmarshal (%s) failed: %v",
			value, err)
		return
	}

	for touchUUID, v := range m.touchscreenMap {
		for _, t := range m.Touchscreens {
			if t.uuid == touchUUID {
				m.TouchMap[t.Serial] = v.OutputName
				break
			}
		}
	}
}

func (m *Manager) doSetTouchMap(monitor0 *Monitor, touchUUID string) error {
	var touchId int32 = -1
	for _, touchscreen := range m.Touchscreens {
		if touchscreen.uuid != touchUUID {
			continue
		}

		touchId = touchscreen.Id
	}
	if touchId == -1 {
		return fmt.Errorf("invalid touchscreen: %s", touchUUID)
	}

	dxTouchscreen, err := dxinput.NewTouchscreen(touchId)
	if err != nil {
		return err
	}

	if monitor0.Enabled {
		matrix := m.genTransformationMatrix(monitor0.X, monitor0.Y, monitor0.Width, monitor0.Height, monitor0.Rotation|monitor0.Reflect)
		logger.Debugf("matrix: %v", matrix)

		err = dxTouchscreen.Enable(true)
		if err != nil {
			return err
		}

		return dxTouchscreen.SetTransformationMatrix(matrix)
	} else {
		logger.Debugf("touchscreen %s disabled", touchUUID)
		return dxTouchscreen.Enable(false)
	}
}

func (m *Manager) updateTouchscreenMap(outputName string, touchUUID string, auto bool) {
	var err error

	m.touchscreenMap[touchUUID] = touchscreenMapValue{
		OutputName: outputName,
		Auto:       auto,
	}
	m.settings.SetString(gsKeyMapOutput, jsonMarshal(m.touchscreenMap))

	var touchSerial string
	for _, v := range m.Touchscreens {
		if v.uuid == touchUUID {
			touchSerial = v.Serial
		}
	}

	m.TouchMap[touchSerial] = outputName

	err = m.emitPropChangedTouchMap(m.TouchMap)
	if err != nil {
		logger.Warning("failed to emit TouchMap PropChanged:", err)
	}
}

func (m *Manager) removeTouchscreenMap(touchUUID string) {
	delete(m.touchscreenMap, touchUUID)
	m.settings.SetString(gsKeyMapOutput, jsonMarshal(m.touchscreenMap))

	var touchSerial string
	for _, v := range m.Touchscreens {
		if v.uuid == touchUUID {
			touchSerial = v.Serial
		}
	}

	delete(m.TouchMap, touchSerial)

	err := m.emitPropChangedTouchMap(m.TouchMap)
	if err != nil {
		logger.Warning("failed to emit TouchMap PropChanged:", err)
	}
}

func (m *Manager) associateTouch(monitor *Monitor, touchUUID string, auto bool) error {
	m.PropsMu.Lock()
	defer m.PropsMu.Unlock()

	if v, ok := m.touchscreenMap[touchUUID]; ok && v.OutputName == monitor.Name {
		return nil
	}

	err := m.doSetTouchMap(monitor, touchUUID)
	if err != nil {
		logger.Warning("[AssociateTouch] set failed:", err)
		return err
	}

	m.updateTouchscreenMap(monitor.Name, touchUUID, auto)

	return nil
}

func (m *Manager) saveConfig() error {
	logger.Debug("save config")
	dir := filepath.Dir(configFile)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(configVersionFile, []byte(configVersion), 0644)
	if err != nil {
		return err
	}

	err = m.config.save(configFile)
	if err != nil {
		return err
	}
	return nil
}

func (m *Manager) showTouchscreenDialog(touchscreenSerial string) error {
	cmd := exec.Command(cmdTouchscreenDialogBin, touchscreenSerial)

	err := cmd.Start()
	if err != nil {
		return err
	}

	go func() {
		err = cmd.Wait()
		if err != nil {
			logger.Debug(err)
		}
	}()
	return nil
}

func (m *Manager) handleTouchscreenChanged() {
	logger.Debugf("touchscreens changed %#v", m.Touchscreens)

	monitors := m.getConnectedMonitors()

	// 清除已拔下触摸屏的配置
	for uuid := range m.touchscreenMap {
		found := false
		for _, touch := range m.Touchscreens {
			if touch.uuid == uuid {
				found = true
				break
			}
		}
		if !found {
			m.removeTouchscreenMap(uuid)
		}
	}

	for _, touch := range m.Touchscreens {
		// 有配置，直接使配置生效
		if v, ok := m.touchscreenMap[touch.uuid]; ok {
			monitor := monitors.GetByName(v.OutputName)
			if monitor != nil {
				logger.Debugf("assigned %s to %s, cfg", touch.uuid, v.OutputName)
				err := m.doSetTouchMap(monitor, touch.uuid)
				if err != nil {
					logger.Warning("failed to map touchscreen:", err)
				}
				continue
			}

			// else 配置中的显示器不存在，忽略配置并删除
			m.removeTouchscreenMap(touch.uuid)
		}

		if touch.outputName != "" {
			logger.Debugf("assigned %s to %s, WL_OUTPUT", touch.uuid, touch.outputName)
			monitor := monitors.GetByName(touch.outputName)
			if monitor == nil {
				logger.Warning("WL_OUTPUT not found")
				continue
			}
			err := m.associateTouch(monitor, touch.uuid, true)
			if err != nil {
				logger.Warning(err)
			}
			continue
		}

		// 物理大小匹配
		assigned := false
		for _, monitor := range monitors {
			logger.Debugf("monitor %s w %d h %d, touch %s w %d h %d",
				monitor.Name, monitor.MmWidth, monitor.MmHeight,
				touch.uuid, uint32(math.Round(touch.width)), uint32(math.Round(touch.height)))

			if monitor.MmWidth == uint32(math.Round(touch.width)) && monitor.MmHeight == uint32(math.Round(touch.height)) {
				logger.Debugf("assigned %s to %s, phy size", touch.uuid, monitor.Name)
				err := m.associateTouch(monitor, touch.uuid, true)
				if err != nil {
					logger.Warning(err)
				}
				assigned = true
				break
			}
		}
		if assigned {
			continue
		}

		// 有内置显示器，且触摸屏不是通过 USB 连接，关联内置显示器
		if m.builtinMonitor != nil {
			if touch.busType != BusTypeUSB {
				logger.Debugf("assigned %s to %s, builtin", touch.uuid, m.builtinMonitor.Name)
				err := m.associateTouch(m.builtinMonitor, touch.uuid, true)
				if err != nil {
					logger.Warning(err)
				}
				continue
			}
		}

		// 关联主显示器，不保存主显示器不保存配置，并显示配置 Dialog
		monitor := monitors.GetByName(m.Primary)
		if monitor == nil {
			logger.Warningf("primary output %s not found", m.Primary)
		} else {
			err := m.doSetTouchMap(monitor, touch.uuid)
			if err != nil {
				logger.Warning("failed to map touchscreen:", err)
			}
		}

		err := m.showTouchscreenDialog(touch.Serial)
		if err != nil {
			logger.Warning("shotTouchscreenOSD", err)
		}
	}
}

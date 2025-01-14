// SPDX-FileCopyrightText: 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package watchdog

import (
	"time"
)

const (
	kWinServiceName = "org.kde.KWin"
	ddeKWinCommand  = "kwin_no_scale"
)

func isDdeKWinRunning() (bool, error) {
	return isDBusServiceExist(kWinServiceName)
}

func launchDdeKWin() error {
	return launchCommand(ddeKWinCommand, nil, "dde-kwin")
}

func newDdeKWinTask() *taskInfo {
	t := newTaskInfo(wmTaskName, isDdeKWinRunning, launchDdeKWin)
	t.launchDelay = 3 * time.Second
	return t
}

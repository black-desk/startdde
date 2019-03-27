/*
 * Copyright (C) 2016 ~ 2018 Deepin Technology Co., Ltd.
 *
 * Author:     jouyouyun <jouyouwen717@gmail.com>
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package watchdog

const (
	ddeDockTaskName    = "dde-dock"
	ddeDockServiceName = "com.deepin.dde.Dock"
)

func isDDEDockRunning() (bool, error) {
	return isDBusServiceExist(ddeDockServiceName)
}

func launchDDEDock() error {
	return startService(ddeDockServiceName)
}

func newDDEDockTask() *taskInfo {
	return newTaskInfo(ddeDockTaskName, isDDEDockRunning, launchDDEDock)
}
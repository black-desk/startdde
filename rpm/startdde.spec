%global _missing_build_ids_terminate_build 0
%global debug_package   %{nil}

%define specrelease 1%{?dist}
%if 0%{?openeuler}
%define specrelease 1
%endif

Name:           startdde
Version:        5.8.9.1
Release:        %{specrelease}
Summary:        Starter of deepin desktop environment
License:        GPLv3
URL:            https://github.com/linuxdeepin/startdde
Source0:        %{name}-%{version}.tar.xz

BuildRequires:  golang
BuildRequires:  jq
BuildRequires:  gocode
BuildRequires:  glib2-devel
BuildRequires:  pkgconfig(x11)
BuildRequires:  libXcursor-devel
BuildRequires:  libXfixes-devel
BuildRequires:  gtk3-devel
BuildRequires:  pulseaudio-libs-devel
BuildRequires:  libgnome-keyring-devel
BuildRequires:  alsa-lib-devel
BuildRequires:  pkgconfig(gudev-1.0)
BuildRequires:  go-gir-generator
BuildRequires:  dde-api-devel
BuildRequires:  go-lib-devel
BuildRequires:  golang-github-linuxdeepin-go-x11-client-devel
BuildRequires:  golang-github-linuxdeepin-go-dbus-factory-devel
BuildRequires:  libsecret-devel

Provides:       x-session-manager
Requires:       dde-daemon
Requires:       procps
Requires:       gocode
Requires:       deepin-desktop-schemas
Requires:       dde-kwin
Requires:       libXfixes
Requires:       libXcursor
Requires:       libsecret
Recommends:     dde-qt5integration

%description
%{summary}.

%prep
%autosetup -n %{name}-%{version}
sed -i 's|/usr/lib/deepin-daemon|/usr/libexec/deepin-daemon|g' \
misc/auto_launch/chinese.json misc/auto_launch/default.json

patch Makefile < rpm/Makefile.patch
patch main.go < rpm/main.go.patch

%build
export GOPATH=/usr/share/gocode

## Scripts in /etc/X11/Xsession.d are not executed after xorg start
sed -i 's|X11/Xsession.d|X11/xinit/xinitrc.d|g' Makefile

%make_build GO_BUILD_FLAGS=-trimpath

%install
%make_install

%post
xsOptsFile=/etc/X11/Xsession.options
update-alternatives --install /usr/bin/x-session-manager x-session-manager \
    /usr/bin/startdde 90 || true
if [ -f $xsOptsFile ];then
	sed -i '/^use-ssh-agent/d' $xsOptsFile
	if ! grep '^no-use-ssh-agent' $xsOptsFile >/dev/null; then
		echo no-use-ssh-agent >> $xsOptsFile
	fi
fi

%files
%doc README.md
%license LICENSE
%{_sysconfdir}/X11/xinit/xinitrc.d/00deepin-dde-env
%{_sysconfdir}/X11/xinit/xinitrc.d/01deepin-profile
%{_sysconfdir}/profile.d/deepin-xdg-dir.sh
%{_bindir}/%{name}
%{_sbindir}/deepin-fix-xauthority-perm
%{_datadir}/xsessions/deepin.desktop
%{_datadir}/lightdm/lightdm.conf.d/60-deepin.conf
%{_datadir}/%{name}/auto_launch.json
%{_datadir}/%{name}/memchecker.json
%{_datadir}/%{name}/app_startup.conf
%{_datadir}/%{name}/filter.conf
%{_datadir}/glib-2.0/schemas/com.deepin.dde.display.gschema.xml
%{_datadir}/glib-2.0/schemas/com.deepin.dde.startdde.gschema.xml
/usr/lib/deepin-daemon/greeter-display-daemon

%changelog
* Tue Apr 13 2021 uoser <uoser@uniontech.com> - 5.8.9.1-1
- update to 5.8.9.1-1

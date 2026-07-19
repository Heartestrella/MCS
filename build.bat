@echo off
rem Build both release flavors:
rem   mcs-panel.exe      - pure server (console window; for headless / remote / autostart)
rem   mcs-panel-gui.exe  - windowed (no console; panel UI embedded via WebView2, closing the window exits)
cd /d "%~dp0"

echo [1/2] build mcs-panel.exe (server)...
go build -trimpath -ldflags="-s -w" -o mcs-panel.exe ./cmd/mcs-panel || exit /b 1

echo [2/2] build mcs-panel-gui.exe (webview)...
go build -trimpath -tags webview -ldflags="-s -w -H windowsgui" -o mcs-panel-gui.exe ./cmd/mcs-panel || exit /b 1

echo done.
dir mcs-panel*.exe | findstr /i "mcs-panel"

@echo off
setlocal
echo ========================================
echo  L4D2 RCON Agent - Linux Build (cross)
echo ========================================
cd /d "%~dp0"
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags="-s -w" -o l4d2-rcon-agent .
if %ERRORLEVEL% equ 0 (
    echo [OK] Build success: l4d2-rcon-agent
) else (
    echo [FAIL] Build failed!
    pause
    exit /b 1
)
endlocal

@echo off
cd /d "%~dp0"
echo Building api-gateway for Windows...
cd src
go build -ldflags="-s -w" -o ..\api-gateway.exe .
if %ERRORLEVEL% equ 0 (
    echo Done: api-gateway.exe
) else (
    echo Build failed.
    exit /b %ERRORLEVEL%
)

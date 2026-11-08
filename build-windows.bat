@echo off
REM Build PhotoClassifierPro for Windows (pure ASCII: GBK console will garble UTF-8)
setlocal
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
set GOPROXY=https://goproxy.cn,direct

echo [1/3] tidy
go mod tidy
if errorlevel 1 goto fail

echo [2/3] test
go test ./...
if errorlevel 1 goto fail

echo [3/3] build (GUI subsystem, no console window)
go build -trimpath -ldflags "-s -w -H=windowsgui" -o PhotoClassifierPro.exe .
if errorlevel 1 goto fail

echo Done: PhotoClassifierPro.exe
dir /b PhotoClassifierPro.exe
goto :eof

:fail
echo BUILD FAILED
exit /b 1

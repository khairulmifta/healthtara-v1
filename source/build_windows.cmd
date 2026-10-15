@echo off
cd /d "%~dp0"
where go >nul 2>nul
if errorlevel 1 (
  echo Untuk pengembang: pasang Go dari https://go.dev/dl terlebih dahulu.
  pause
  exit /b 1
)
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go test -buildvcs=false .
if errorlevel 1 exit /b 1
go build -buildvcs=false -trimpath -ldflags="-H windowsgui -s -w" -o ..\Healthtara.exe .
if errorlevel 1 exit /b 1
go build -buildvcs=false -trimpath -ldflags="-H windowsgui -s -w" -o ..\Gabungkan_Healthtara.exe .\cmd\merge
echo Build selesai. Pengguna cukup membuka EXE; tidak perlu Go.
pause

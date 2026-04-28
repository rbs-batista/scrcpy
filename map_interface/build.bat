@echo off
setlocal

:: Generate icon.ico if not present
if not exist icon.ico (
    echo Generating icon.ico...
    go run gen_icon.go
)

:: Generate resource.syso (icon + version info) — uses go run to avoid PATH issues
echo Embedding icon...
go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -64 -o resource.syso
if %errorlevel% neq 0 (
    echo WARNING: icon embedding failed, building without icon.
    if exist resource.syso del resource.syso
)

:: Build without console window
go build -ldflags="-H windowsgui" -o "DM Tools.exe" .
if %errorlevel% neq 0 (
    echo Build failed.
    pause
) else (
    echo Build OK: DM Tools.exe
)

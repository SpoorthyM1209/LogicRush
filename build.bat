@echo off
set "GO_BIN=go"
where go >nul 2>nul
if %errorlevel% neq 0 (
    if exist "%LOCALAPPDATA%\GoSDK\go\bin\go.exe" (
        set "GO_BIN=%LOCALAPPDATA%\GoSDK\go\bin\go.exe"
    )
)

echo Building kvdb.exe and torture.exe using %GO_BIN%...
"%GO_BIN%" build -o kvdb.exe ./src/cmd/kvdb
if %errorlevel% neq 0 exit /b %errorlevel%
"%GO_BIN%" build -o torture.exe ./torture
if %errorlevel% neq 0 exit /b %errorlevel%
echo Build complete!

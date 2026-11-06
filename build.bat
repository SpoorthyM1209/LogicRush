@echo off
echo Building kvdb.exe and torture.exe...
go build -o kvdb.exe ./src/cmd/kvdb
if %errorlevel% neq 0 exit /b %errorlevel%
go build -o torture.exe ./torture
if %errorlevel% neq 0 exit /b %errorlevel%
echo Build complete!

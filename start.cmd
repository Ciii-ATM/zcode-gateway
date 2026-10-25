@echo off
cd /d "%~dp0"
if not exist zcode-gateway.exe (
  go build -o zcode-gateway.exe .
)
zcode-gateway.exe

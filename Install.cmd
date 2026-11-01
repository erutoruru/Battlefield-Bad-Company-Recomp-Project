@echo off
rem Battlefield: Bad Company PC - one-time install (builds bfbc.exe from your own game files)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\install.ps1" %*
echo.
pause

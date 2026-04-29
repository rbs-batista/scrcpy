@echo off
title ADB / SCRCPY CLEANUP

echo ================================
echo RESETANDO ADB
echo ================================
adb kill-server
adb start-server

echo.
echo ================================
echo REMOVENDO FORWARDS
echo ================================
adb forward --remove-all

echo.
echo ================================
echo MATANDO SCRCPY NO DEVICE
echo ================================
adb shell "pkill -9 -f scrcpy"

echo.
echo ================================
echo 🧹 REMOVENDO SERVER ANTIGO
echo ================================
adb shell rm -f /data/local/tmp/scrcpy-server

echo.
echo ================================
echo REINICIANDO USB
echo ================================
adb usb

echo.
echo ================================
echo LISTA DE DEVICES
echo ================================
adb devices

echo.
echo ================================
echo VERIFICANDO FORWARDS ATIVOS
echo ================================
adb forward --list

echo.
echo ================================
echo VERIFICANDO PORTA 27183
echo ================================
netstat -ano | findstr 27183

echo.
echo ================================
echo LIMPEZA FINALIZADA
echo ================================

pause
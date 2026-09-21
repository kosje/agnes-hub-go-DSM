@echo off
chcp 936 >nul
setlocal enabledelayedexpansion
cd /d "%~dp0"
title baipiao-hub

set "BIN=baipiao-hub.exe"
set "HOST=127.0.0.1"
set "PORT=4142"
set "DATADIR=%~dp0data"

echo ============================================================
echo   baipiao-hub   Agnes AI 多账号聚合中转 + RPM 限流排队网关
echo ============================================================
echo.

rem 端口预检：Python 版 agnes-hub 默认也用 4142，两者同时启动会直接 bind 失败，
rem 而 Go 版抛的是系统英文原文，不容易看出撞了谁，所以这里先给出中文指引。
netstat -ano | findstr ":%PORT% " | findstr "LISTENING" >nul
if not errorlevel 1 goto PORTBUSY

if exist "%BIN%" goto RUN

echo 未找到 %BIN%，尝试用本机 Go 工具链编译...
where go >nul 2>nul
if errorlevel 1 goto NOGO

if "%GOPATH%"=="" set "GOPATH=%USERPROFILE%\gopath"
set "GOTOOLCHAIN=local"
go build -trimpath -ldflags "-s -w" -o "%BIN%" .
if errorlevel 1 goto BUILDFAIL
echo 编译完成。
echo.
goto RUN

:PORTBUSY
echo [警告] 端口 %PORT% 已被占用，无法启动。
echo.
echo        占用该端口的记录如下:
netstat -ano | findstr ":%PORT%"
echo.
echo        处理方式二选一:
echo          1. 先关掉占用该端口的程序，再重新双击本文件
echo             若之前跑过 Python 版 agnes-hub，它默认也用 4142
echo          2. 用记事本打开本文件，把 set "PORT=%PORT%" 改成别的端口
echo             例如 set "PORT=4143"
echo.
pause
exit /b 1

:NOGO
echo [错误] 本机没有 go 命令，当前目录也没有编译好的 %BIN%。
echo        请二选一:
echo          1. 安装 Go 1.22 以上版本，再重新双击本文件
echo          2. 把编译好的 %BIN% 放到本文件所在目录
echo.
pause
exit /b 1

:BUILDFAIL
echo [错误] 编译失败，请查看上方 Go 输出的错误信息。
echo.
pause
exit /b 1

:RUN
echo 控制台    http://%HOST%:%PORT%/console      首次登录请设置管理员密码
echo 接口基址  http://%HOST%:%PORT%/v1
echo 统一模型  agnes-auto   文本 / 生图 / 生视频 自动判定
echo 数据目录  %DATADIR%
echo.
echo 关闭本窗口即停止服务。
echo.

rem 显式清零 ERRORLEVEL：前面的 findstr 预检在「没匹配到」时会留下 1，
rem 而 echo 并不重置 ERRORLEVEL，不清理就会把一个与本次运行无关的退出码报给用户。
ver >nul
"%BIN%" -host %HOST% -port %PORT% -data "%DATADIR%"
set "CODE=%ERRORLEVEL%"

echo.
echo baipiao-hub 已退出，退出码 %CODE%。
pause
exit /b %CODE%

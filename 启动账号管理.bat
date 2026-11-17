chcp 65001 >nul
title muse.ai 账号管理系统
cd /d "%~dp0"

if not exist "muse-manager.exe" (
    echo [错误] 未找到 muse-manager.exe，请确保批处理与程序放在同一目录。
    echo.
    pause
    exit /b 1
)

echo ==============================================
echo        muse.ai 账号管理系统  一键启动
echo ==============================================
echo.
set "MUSE_PROXY=socks5://127.0.0.1:10808"
echo 程序目录 : %cd%
echo 数据目录 : %cd%\data
echo 访问地址 : http://127.0.0.1:8787
echo 全局代理 : %MUSE_PROXY%   （拉取邀请信息使用；若端口不同请修改本行，删除本行则为直连）
echo.
echo [提示] 浏览器将自动打开；关闭本窗口即停止服务。
echo [提示] 需要自定义端口可这样启动:  启动账号管理.bat -addr 127.0.0.1:9000
echo.

muse-manager.exe serve -open 

echo.
echo 服务已退出。
pause

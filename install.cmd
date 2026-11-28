@echo off
setlocal enabledelayedexpansion

set "REPO=Tiggy-Chan/agy-pro"
set "INSTALL_DIR=%LOCALAPPDATA%\agy\bin"
if defined AGY_PRO_INSTALL_DIR set "INSTALL_DIR=%AGY_PRO_INSTALL_DIR%"
set "VERSION=%AGY_PRO_VERSION%"

if /i "%PROCESSOR_ARCHITECTURE%"=="AMD64" (
    set "ARCH=amd64"
) else if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" (
    set "ARCH=arm64"
) else (
    echo error: unsupported architecture %PROCESSOR_ARCHITECTURE% >&2
    exit /b 1
)

if "%VERSION%"=="" (
    for /f "delims=" %%U in ('curl -fsSL -o nul -w "%%{url_effective}" "https://github.com/%REPO%/releases/latest"') do set "FINAL_URL=%%U"
    for %%T in ("!FINAL_URL:/=" "!") do set "VERSION=%%~T"
)
if "%VERSION%"=="" (
    echo error: failed to resolve latest release tag for %REPO% >&2
    exit /b 1
)

set "ASSET=agy-pro-windows-%ARCH%.exe"
set "BASE_URL=https://github.com/%REPO%/releases/download/%VERSION%"
set "TMP_DIR=%TEMP%\agy-pro-install-%RANDOM%%RANDOM%"
mkdir "%TMP_DIR%" || exit /b 1

echo Downloading agy-pro %VERSION% (windows/%ARCH%)...
curl -fsSL "%BASE_URL%/%ASSET%" -o "%TMP_DIR%\%ASSET%" || goto :fail
curl -fsSL "%BASE_URL%/checksums.txt" -o "%TMP_DIR%\checksums.txt" || goto :fail

set "EXPECTED="
for /f "tokens=1,2" %%A in ('findstr /r /c:"  %ASSET%$" "%TMP_DIR%\checksums.txt"') do set "EXPECTED=%%A"
if "%EXPECTED%"=="" (
    echo error: checksum entry for %ASSET% not found >&2
    goto :fail
)

set "ACTUAL="
for /f "skip=1 delims=" %%H in ('certutil -hashfile "%TMP_DIR%\%ASSET%" SHA256') do (
    if not defined ACTUAL set "ACTUAL=%%H"
)
set "ACTUAL=%ACTUAL: =%"
if /i not "%ACTUAL%"=="%EXPECTED%" (
    echo error: SHA-256 verification failed for %ASSET% >&2
    goto :fail
)

if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%" || goto :fail
copy /y "%TMP_DIR%\%ASSET%" "%INSTALL_DIR%\agy-pro.exe" >nul || goto :fail
rmdir /s /q "%TMP_DIR%" 2>nul

echo Installed agy-pro %VERSION% -^> %INSTALL_DIR%\agy-pro.exe
exit /b 0

:fail
rmdir /s /q "%TMP_DIR%" 2>nul
exit /b 1

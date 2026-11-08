# PhotoClassifierPro

> **定位：预览版（Preview）。**
> 新功能先在这里落地，可能不稳定 —— 这是刻意的。验证过的改动会依次同步到
> [photo-classifier](https://github.com/shenglihalisen/photo-classifier)（标准版）和
> [photo-classifier-stable](https://github.com/shenglihalisen/photo-classifier-stable)（保守版）。
> 想要「能干活」而不是「最新」的话，用标准版。

照片质量批量筛片工具。**一个 exe，零依赖**——不需要 Python、不需要 OpenCV、不需要 ffmpeg。

双击就是一个**原生窗口**（WebView2，不是浏览器标签页），选一个目录，它会把损坏、空镜、模糊、噪点、曝光异常、镜头脏污、重复、闭眼这八类问题挑出来，标上徽章给你看，**绝不改动或删除你的照片**。

检测算法与阈值来自 [medreview](https://github.com/shenglihalisen/medreview) 的 QC v5，那套数值是在真实素材上校准过的。

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)]()
[![License](https://img.shields.io/badge/license-MIT-blue)]()

</div>

---

## 1. 为什么是 Go 重写

这个项目原本是 Python（OpenCV + MediaPipe + Flask + PyQt5）。Go 重写解决了三个实际问题：

| 老问题 | 现在 |
|---|---|
| PyInstaller 打包 200MB+，还要挑 Python 版本 | 单个 12MB 可执行文件 |
| MediaPipe 在 Python 3.14 上装不上（Pro 版为此弃用它） | 纯 Go 实现 Haar 级联 + 边缘密度，无第三方运行时 |
| 检测算法和 medreview 各写一份，数值对不上 | 算法同源，测量口径与阈值一致 |
| 扫描几千张照片慢且吃内存 | 并发解码，8 路流水线 |

## 2. 检测器

| 检测器 | 判据 | 输出 |
|---|---|---|
| 损坏 corrupted | 解码失败 / 文件截断 | 布尔 + 错误原因 |
| 空镜 blank | 全局灰度标准差 < 8 | 标准差 |
| 模糊 blur | 长边 1024 灰度上的拉普拉斯**标准差** < 15 | 标准差 |
| 噪点 noise | 16px 块中最平坦 10% 的标准差（噪声底线）> 5 | 噪声底线 |
| 欠曝 / 过曝 exposure | 均值 < 100 且死黑占比 > 35%；均值 > 150 且死白占比 > 80% | 均值 + 两端占比 |
| 镜头脏污 lens-dirt | 8×8 网格内「低方差 + 邻域亮度突兀」的成团异常块 | 置信度 + 红框位置 |
| 重复 duplicate | pHash 汉明 ≤ 5 **且** 像素差异 ≤ 5% | 重复组编号 |
| 闭眼 blink | Haar 检人脸 → 眼部区域 Sobel 边缘密度 < 0.10（**两眼都低**才判） | 人脸数 / 闭眼数 |

> 阈值默认值遵循「宁漏勿滥」——真实素材两侧留 2~3 倍余量。所有阈值可在界面右侧「阈值」抽屉里实时调，改完重新扫描生效。

## 3. 快速开始

### 用发行版

下载 `PhotoClassifierPro.exe`，双击 —— 直接开一个原生窗口，没有黑窗、没有浏览器标签页。

窗口内置的是本地服务页面（`127.0.0.1` 随机端口，只在本机可达）。想要浏览器标签页的用 `-no-gui`。

### 从源码构建

```bash
# 需要 Go 1.24+
go build -trimpath -ldflags "-s -w -H=windowsgui" -o PhotoClassifierPro.exe .

# 或者用批处理（Windows，自动跑测试）
build-windows.bat
```

Linux / macOS：

```bash
go build -trimpath -ldflags "-s -w" -o photo-classifier .
./photo-classifier -port 8080
```

## 4. 三种用法

**原生窗口（默认）**

```bash
PhotoClassifierPro.exe                 # 直接开窗口
PhotoClassifierPro.exe -port 9000      # 指定端口（被占用会自动换一个）
PhotoClassifierPro.exe -no-gui         # 改用默认浏览器
PhotoClassifierPro.exe -no-gui -no-browser   # 只起服务，不开任何界面
```

**命令行扫描**

```bash
PhotoClassifierPro.exe -scan D:/photos            # 打印报告
PhotoClassifierPro.exe -scan D:/photos -csv out.csv  # 导出 CSV
PhotoClassifierPro.exe -scan D:/photos -json      # JSON 输出
PhotoClassifierPro.exe -scan D:/photos -move      # 把废片移到 _待整理/<类型>/
PhotoClassifierPro.exe -scan D:/photos -copy      # 复制而不是移动
PhotoClassifierPro.exe -scan D:/photos -face=false  # 关掉闭眼检测（更快）
```

**整理动作的安全约束**

- 只移动/复制，**永不删除**
- 目标固定在源目录下的 `_待整理/`，同名文件自动加 `_1`、`_2`，不覆盖
- 默认只出报告不动文件，`-move` / `-copy` 才真的动
- 只由「重复」触发的项默认不归档——删哪张该由人决定

## 5. 阈值

八个检测器的阈值都能运行时调整。默认值来自 medreview 在真实素材上的校准：

| 参数 | 默认 | 范围 | 方向 |
|---|---|---|---|
| `blurLapStd` | 15.0 | 5–60 | 调大更严 |
| `noiseFloor` | 5.0 | 1–20 | 调小更严 |
| `blankStd` | 8.0 | 2–30 | 调大更严 |
| `expUnder` | 100 | 30–160 | 调大更严 |
| `expOver` | 150 | 90–220 | 调小更严 |
| `dupSim` | 0.95 | 0.5–1.0 | 调大更严 |
| `blinkEdge` | 0.10 | 0.03–0.30 | 调大更严 |

越界会被钳回范围（拖到头就是边界值，不会报错）。阈值改动会改变结果版本号，旧结果自动失效重算。

## 6. 与 Python 版的差异

| | Python 版 v2.0.4 | Go 版 v3.0.0 |
|---|---|---|
| 分发 | PyInstaller，200MB+ | 单文件 12MB |
| 依赖 | OpenCV / MediaPipe / Flask / PyQt5 | 仅标准库 + x/image |
| 检测器 | 5 个 | 8 个（+噪点/曝光/重复/脏污） |
| 阈值 | 硬编码 | 可运行时调 + 范围钳制 |
| 闭眼 | MediaPipe 关键点 / Haar+Canny | 纯 Go Haar + Sobel 边缘密度 |
| 重复 | 无 | pHash + 像素二次确认 |
| 界面 | PyQt5 + Flask 两套 | 一套网页界面：原生窗口（WebView2）或浏览器，CLI 共用内核 |
| 性能 | 单线程 | 并发解码（默认 ≤8 路） |

## 7. 隐私与安全

**本机模式（默认，监听 127.0.0.1）**
- 不上传任何照片，没有任何外部网络请求。
- 图片接口做了路径 + 扩展名双重校验，只能读当前扫描根内的图片。
- 源文件全程只读（除非你显式点「归档」或加 `-move`）。

**对外模式（`-addr 0.0.0.0`，容器/局域网）**
- **自动启用口令认证**：口令来自 `-pass` 或环境变量 `PCP_PASS`，都没给就启动时随机生成并打印到日志。所有 `/api/*` 都要带会话 Cookie。
- 强烈建议同时加 `-base-dir <照片目录>`，把 `/api/scan` 的扫描范围锁死在该目录内；不加会有启动警告。
- stable 仓库的 Dockerfile 已经固定 `-base-dir /data`，compose 里可设 `PCP_PASS`。
- 这是 HTTP 明文服务，只适合可信局域网；要上公网请自己套 HTTPS 反代。

## 8. 已知限制

- **不支持 HEIC/HEIF 和相机 RAW（CR2/CR3/NEF/ARW）**。这两类需要 libheif / libraw 的 cgo 绑定，会破坏 `CGO_ENABLED=0` 的单文件前提。需要的话请先用工具转成 JPEG。
- 闭眼检测基于 Haar 正脸模型，**侧脸、低头、遮挡严重的人脸检不出**。这是 Viola-Jones 的固有限制，不是 bug。
- 闭眼阈值（0.10）需要在你的真实素材上再校准一次。合成素材只能验证方向。
- 单次扫描内的重复检测默认开启；跨扫描的**全库去重**要显式勾「全库去重」或加 `-global-dup`，结果是候选（需人工确认）。
- 界面是单用户的：同一时间只服务一个扫描任务。
- 界面中英双语（顶栏 EN/中 切换，`?lang=en` 可固定英文）；结果页分页渲染，每页 60 张。
- 原生窗口需要 **WebView2 运行时**（Win10/11 随 Edge 预装，Win7/8 需自己装）。没有时会自动回退到浏览器，不会闪退。

## 9. 项目结构

```
PhotoClassifierPro/
├── main.go              入口（窗口 / 浏览器 / CLI）
├── gui_windows.go       WebView2 原生窗口（纯 Go，无 cgo）
├── gui_other.go         非 Windows 平台回退
├── qc/                  检测算法（与 medreview QC v5 同源）
│   ├── qc.go            七个图像检测器
│   ├── decode.go        纯 Go 解码 + EXIF 转正 + 双尺度灰度
│   ├── analyzer.go      检测流水线
│   └── thresholds.go    阈值 + 钳制 + 版本指纹
├── face/                人脸与闭眼（纯 Go）
│   ├── cascade.go       Haar 级联解析 + 金字塔检测
│   ├── blink.go         眼部边缘密度判闭眼
│   ├── skin.go          肤色预筛
│   └── data/            内嵌 haarcascade 模型（528KB）
├── engine/              批量扫描、跨文件去重、归档
├── web/                 HTTP API + 内嵌前端
└── cmd/gentestdata/     合成测试素材生成器
```

## 10. 许可证

MIT License。

第三方组件：
- [golang.org/x/image](https://golang.org/x/image)（BSD-3-Clause）：webp / tiff / bmp 解码。
- `haarcascade_frontalface_alt2.xml`（[OpenCV](https://opencv.org/)，Apache-2.0 / 3-clause BSD）：人脸级联模型，随仓库分发。

## 11. 界面

浏览器 / 窗口界面分两屏：

- **首屏**：大标题 + 路径输入卡，下方列出四项保证与八个检测器。没有结果时不摆空表格。
- **结果页**：一条紧凑统计栏（总数 / 质量问题 / 重复 / 损坏 / 正常）→ 分段筛选 → 缩略图网格。点任意缩略图进入预览，可滚轮缩放、拖拽平移、← → 切换、`0` 适应窗口、`Esc` 关闭；镜头脏污会以红框叠在原图对应位置。

路径框会记住上一次扫描的目录（存在 localStorage）。想让某个目录「打开即扫描」，用 `?root=D:/photos`。

---

# English

Photo triage tool. **One exe, zero dependencies** — no Python, no OpenCV, no ffmpeg.

A **native window** (WebView2 — not a browser tab) opens on double-click. Pick a folder and it flags eight kinds of problems: corrupted, blank, blurry, noisy, badly exposed, lens-dirt, duplicates, and closed eyes. It **never modifies or deletes your photos**.

The detectors and thresholds come from [medreview](https://github.com/shenglihalisen/medreview)'s QC v5, calibrated on real material.

## 1. Detectors

| Detector | Criterion | Output |
|---|---|---|
| corrupted | decode failure / truncated file | boolean + reason |
| blank | global gray std < 8 | std |
| blur | Laplacian **std** on a 1024px-long-edge gray < 15 | std |
| noise | std of the flattest 10% of 16px blocks (noise floor) > 5 | floor |
| exposure | mean < 100 and clipped-black > 35%; mean > 150 and clipped-white > 80% | mean + clip ratios |
| lens-dirt | clustered low-variance blocks with abrupt local brightness in an 8×8 grid | confidence + boxes |
| duplicate | pHash Hamming ≤ 5 **and** pixel diff ≤ 5% | group id |
| blink | Haar face → eye-region Sobel edge density < 0.10 (**both eyes** must fail) | faces / closed |

Thresholds default to the conservative side ("better a miss than a false alarm") with 2–3× margin on both ends. All of them are adjustable at runtime from the **Thresholds** drawer; re-scan to apply.

## 2. Quick start

```bash
git clone https://github.com/shenglihalisen/PhotoClassifierPro.git
cd PhotoClassifierPro
go build -trimpath -ldflags "-s -w -H=windowsgui" -o PhotoClassifierPro.exe .
```

Run without arguments to get the native window. No WebView2 runtime? It falls back to your default browser automatically.

```bash
PhotoClassifierPro.exe -no-gui         # browser tab instead of a native window
PhotoClassifierPro.exe -port 9000      # fixed port (falls back to a random one if busy)
```

```bash
PhotoClassifierPro.exe -scan D:/photos              # report
PhotoClassifierPro.exe -scan D:/photos -csv out.csv # export CSV
PhotoClassifierPro.exe -scan D:/photos -move        # move rejects into _待整理/<type>/
PhotoClassifierPro.exe -scan D:/photos -face=false  # skip the blink detector
```

## 3. Safety guarantees

- Only binds to `127.0.0.1`. Not reachable from the LAN.
- Uploads nothing. No network calls beyond opening your local browser.
- Thumbnail API validates every path against the current scan root.
- Source files are read-only unless you explicitly archive.
- Archiving **moves or copies, never deletes**; collisions get `_1`, `_2` suffixes.

## 4. Differences from the Python version

| | Python v2.0.4 | Go v3.0.0 |
|---|---|---|
| Distribution | PyInstaller, 200MB+ | 12MB single binary |
| Dependencies | OpenCV / MediaPipe / Flask / PyQt5 | stdlib + x/image only |
| Detectors | 5 | 8 (+noise/exposure/dup/lens-dirt) |
| Thresholds | hard-coded | runtime-adjustable with clamping |
| Blink | MediaPipe landmarks / Haar+Canny | pure-Go Haar + Sobel edge density |
| Duplicates | none | pHash + pixel double-check |
| UI | PyQt5 + Flask | one web UI: native window (WebView2) or browser; CLI shares the same core |
| Concurrency | single-threaded | parallel decode (≤8 workers) |

## 5. Known limitations

- **No HEIC/HEIF or camera RAW** (CR2/CR3/NEF/ARW). Both need cgo bindings (libheif/libraw) that would break the `CGO_ENABLED=0` single-binary property. Convert to JPEG first.
- The blink detector uses a frontal-face Haar cascade; **profiles, tilted heads and heavily occluded faces are missed**. That's a Viola-Jones limitation, not a bug.
- The blink threshold (0.10) still wants calibration on your own material — synthetic images can only validate direction.
- Duplicate detection is per-scan, not a global library dedupe.
- The UI is single-user: one scan at a time.
- The native window needs the **WebView2 runtime** (preinstalled on Win10/11 with Edge). Missing runtime falls back to the browser instead of failing.

## 6. License

MIT. Third-party: [golang.org/x/image](https://golang.org/x/image) (BSD-3-Clause), `haarcascade_frontalface_alt2.xml` from [OpenCV](https://opencv.org/) (Apache-2.0 / 3-clause BSD).

## 6. Interface

Two screens:

- **Landing**: headline, path input card, four guarantees and the eight detector chips. No empty table before the first scan.
- **Results**: a compact stat bar (total / issues / duplicates / corrupted / clean) → segmented filters → thumbnail grid. Click a thumbnail to open the viewer: wheel to zoom, drag to pan, `←`/`→` to step, `0` to fit, `Esc` to close. Lens-dirt boxes are overlaid on the image in red.

The path field remembers your last folder (localStorage). To make a folder "open and scan", use `?root=D://photos`.

## 12. 附加工具与参数

```bash
# 全库去重（跨扫描比对，索引存在用户配置目录）
PhotoClassifierPro.exe -scan D://photos -global-dup

# 对外提供服务的完整参数
PhotoClassifierPro.exe -no-gui -addr 0.0.0.0 -port 8080 -base-dir D://photos -pass 你的口令

# 用真实人像校准闭眼阈值（把睁眼、闭眼照片分放两个目录）
go run ./cmd/calibrate -open ./open -closed ./closed
go run ./cmd/calibrate -scan ./portraits     # 无标注，只看分布

# 打开即扫描 / 固定英文界面
http://127.0.0.1:8080/?root=D://photos&lang=en
```

三个仓库共用一套内核；`scripts/sync-siblings.sh <repos 目录>` 把预览通道（Pro）的共享代码同步到标准版和保守版。

## 13. License

MIT，见 [LICENSE](./LICENSE)。

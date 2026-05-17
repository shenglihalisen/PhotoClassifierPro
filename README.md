# PhotoClassifierPro 照片自动分类工具
# 该项目为PhotoClassifier优化版，原项目请见(https://github.com/shenglihalisen/PhotoClassifier)
智能检测损坏、空镜、闭眼、模糊、遮挡等缺陷照片，一键整理您的相册。

[![GitHub release\](https://img.shields.io/github/v/release/shenglihalisen/PhotoClassifierPro)](https://github.com/shenglihalisen/PhotoClassifierPro/releases)
[![GitHub last commit\](https://img.shields.io/github/last-commit/shenglihalisen/PhotoClassifierPro)](https://github.com/shenglihalisen/PhotoClassifierPro/commits/main)

## 功能特性

- **5 种缺陷检测**：损坏、空镜、闭眼、模糊、遮挡
- **双端支持**：PyQt5 桌面端 + Flask Web 端
- **两种模式**：自动模式（一键处理）/ 手动确认（逐张审核）
- **批量处理**：支持文件夹批量扫描和文件拖拽上传
- **灵活操作**：自动将废片按缺陷类型分类到子文件夹（损坏/空镜/闭眼/模糊/遮挡）

## 支持的图片格式

JPG、JPEG、PNG、BMP、TIFF、WebP、GIF、HEIC、HEIF

## 安装

### 从源码运行

`ash
# 克隆仓库
git clone https://github.com/shenglihalisen/PhotoClassifierPro.git
cd PhotoClassifierPro

# 安装依赖
pip install -r requirements.txt

# 运行桌面端
python run_desktop.py

# 运行 Web 端
python run_web.py
`

Web 端启动后访问 http://localhost:5000

### 下载发行版

前往 [Releases](https://github.com/shenglihalisen/PhotoClassifierPro/releases) 页面下载对应平台的可执行文件。

| 文件 | 说明 |
|------|------|
| PhotoClassifierPro-Desktop.exe | 桌面端（PyQt5），无需浏览器 |
| PhotoClassifierPro-Web.exe | Web 端（Flask），启动后浏览器访问使用 |

## 使用方法

### Web 端

1. 打开浏览器访问 http://localhost:5000
2. 选择「上传文件」或「选择文件夹」上传要扫描的照片
3. 选择扫描模式（自动 / 手动确认）
4. 点击「开始扫描」
5. 查看扫描结果，对废片进行移动或删除操作

### 桌面端

1. 运行 PhotoClassifierPro-Desktop.exe 或 python run_desktop.py
2. 选择要扫描的文件夹
3. 等待扫描完成
4. 查看分类结果并处理废片

## 检测器说明

| 检测器 | 检测内容 | 技术原理 |
|--------|----------|----------|
| 损坏检测 | 文件损坏、无法打开、截断的图片 | 文件头魔数验证 + PIL 解码尝试 |
| 空镜检测 | 纯色、全黑、全白、大面积单一颜色的照片 | 颜色直方图 + 亮度均方差分析 |
| 闭眼检测 | 人物闭眼的照片 | OpenCV Haar + Canny 边缘密度分析 |
| 模糊检测 | 对焦不准、运动模糊的照片 | 拉普拉斯方差 + Sobel 边缘强度综合评分 |
| 遮挡检测 | 镜头被遮挡的照片 | 信息熵 + 低对比度区域分析 |

## 项目结构

`
PhotoClassifierPro/
├── classifiers/          # 缺陷检测器
│   ├── base.py           # 基类和类型定义
│   ├── corrupted.py      # 损坏检测
│   ├── empty.py          # 空镜检测
│   ├── blink.py          # 闭眼检测
│   ├── blur.py           # 模糊检测
│   └── obstruction.py    # 遮挡检测
├── engine/               # 分类引擎
│   └── classifier.py     # 统一分类引擎
├── desktop/              # 桌面端
│   └── app.py            # PyQt5 界面
├── web/                  # Web 端
│   ├── app.py            # Flask 应用（含安全防护/频率限制/CSRF保护）
│   └── templates/        # HTML 模板（Layui 前端界面）
├── run_desktop.py        # 桌面端入口
├── run_web.py            # Web 端入口
├── requirements.txt      # Python 依赖
├── CHANGELOG.md          # 更新日志
└── README.md             # 本文件
`

## 技术栈

- **图像处理**：OpenCV、Pillow、NumPy
- **桌面端**：PyQt5
- **Web 端**：Flask + Layui
- **打包**：PyInstaller

## 安全特性（Web 端）

- 文件上传大小限制（单文件 50MB，总计 200MB）
- 文件类型白名单验证（文件头魔数检查）
- CSRF Token 防护
- 请求频率限制
- 路径遍历攻击防护
- 删除操作二次确认 Token 机制
- 错误信息脱敏
- 临时文件自动清理

## 更新日志

查看 [CHANGELOG.md](CHANGELOG.md) 了解详细更新记录。

## 许可证

MIT License

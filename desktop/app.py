# -*- coding: utf-8 -*-
"""
照片自动分类工具 - 桌面端应用（简洁版）

功能概述：
    - 简洁 Fusion 风格 UI，标准系统控件
    - 拖拽文件夹支持、扫描中停止
    - 右键菜单、双击打开文件位置
    - 导出 TXT/JSON 报告、窗口状态记忆
    - 路径安全验证、文件名安全过滤、操作确认机制
    - 关于对话框、预览对话框支持缩放
    - 分批加载扫描结果，防止大量图片时主线程冻结

作者: PhotoClassifierPro Team
版本: v2.0.0
"""

import os
import sys
import re
import json
import subprocess
from datetime import datetime

from PyQt5.QtWidgets import (
    QApplication, QMainWindow, QWidget, QVBoxLayout, QHBoxLayout,
    QPushButton, QLabel, QProgressBar, QListWidget, QListWidgetItem,
    QSplitter, QToolBar, QAction, QFileDialog, QMessageBox,
    QGroupBox, QTextEdit, QStatusBar, QMenu,
    QAbstractItemView, QFrame, QDialog, QSizePolicy
)
from PyQt5.QtCore import (
    Qt, QThread, pyqtSignal, QSize, QTimer, QSettings, QMimeData, QPoint
)
from PyQt5.QtGui import QPixmap, QIcon, QFont, QDragEnterEvent, QDropEvent

# 将项目根目录加入 Python 路径
PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from engine.classifier import PhotoClassifier
from classifiers.base import DefectType

# ============================================================
# 常量定义
# ============================================================

APP_NAME = "照片自动分类工具"
APP_VERSION = "v2.0.0"
APP_AUTHOR = "PhotoClassifierPro Team"

# 禁止访问的系统目录
FORBIDDEN_PATHS = [
    os.path.normpath("C:\\Windows"),
    os.path.normpath("C:\\Program Files"),
    os.path.normpath("C:\\Program Files (x86)"),
    os.path.normpath("C:\\System32"),
    "/etc",
    "/usr",
    "/bin",
    "/sbin",
    "/System",
]

# 缺陷类型中文映射
DEFECT_TYPE_NAMES = {
    DefectType.CORRUPTED: "损坏",
    DefectType.EMPTY: "空镜",
    DefectType.BLINK: "闭眼",
    DefectType.BLURRY: "模糊",
    DefectType.OBSTRUCTION: "遮挡",
}

# 缺陷类型英文值映射（用于报告导出）
DEFECT_TYPE_VALUE_NAMES = {
    "corrupted": "损坏",
    "empty": "空镜",
    "blink": "闭眼",
    "blurry": "模糊",
    "obstruction": "遮挡",
}

# QSettings 组织名和应用名（用于窗口状态记忆）
SETTINGS_ORG = "PhotoClassifierPro"
SETTINGS_APP = "DesktopApp"

# 缩略图最大尺寸
THUMBNAIL_MAX_SIZE = 80

# 分批处理参数
BATCH_SIZE = 5
BATCH_INTERVAL_MS = 50


# ============================================================
# 安全工具函数
# ============================================================

def is_path_safe(path: str) -> bool:
    """
    验证路径安全性，防止路径遍历攻击

    检查项：
        1. 路径是否包含 .. 逃逸
        2. 路径是否指向系统敏感目录

    参数:
        path: 待验证的路径字符串

    返回:
        bool - 路径安全返回 True，否则返回 False
    """
    if not path:
        return False

    # 检查原始路径中是否包含路径遍历（在normpath之前检查）
    if ".." in path.replace("/", os.sep).split(os.sep):
        return False

    normalized = os.path.normpath(path)

    # 检查是否指向系统敏感目录
    normalized_lower = normalized.lower()
    for forbidden in FORBIDDEN_PATHS:
        forbidden_lower = os.path.normpath(forbidden).lower()
        if normalized_lower == forbidden_lower or normalized_lower.startswith(forbidden_lower + os.sep):
            return False

    return True


def sanitize_filename(filename: str) -> str:
    """
    过滤文件名中的特殊字符，确保文件名安全

    参数:
        filename: 原始文件名

    返回:
        str - 安全处理后的文件名
    """
    sanitized = re.sub(r'[\\/:*?"<>|]', '_', filename)
    sanitized = re.sub(r'[\x00-\x1f\x7f]', '', sanitized)
    sanitized = sanitized.strip(' .')
    if not sanitized:
        sanitized = "unnamed"
    return sanitized


def mask_path(path: str) -> str:
    """
    对路径进行脱敏处理，隐藏完整路径中的敏感信息

    参数:
        path: 完整文件路径

    返回:
        str - 脱敏后的路径字符串
    """
    if not path:
        return ""
    basename = os.path.basename(path)
    parent = os.path.basename(os.path.dirname(path))
    return os.path.join(parent, basename)


# ============================================================
# 自定义控件
# ============================================================

class PhotoListWidget(QListWidget):
    """
    带缩略图的照片列表控件

    支持图标模式显示照片缩略图，双击发送信号。
    """
    # 双击信号
    item_double_clicked = pyqtSignal(str)

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setIconSize(QSize(80, 80))
        self.setGridSize(QSize(200, 100))
        self.setResizeMode(QListWidget.Adjust)
        self.setViewMode(QListWidget.IconMode)
        self.setMovement(QListWidget.Static)
        self.setSelectionMode(QAbstractItemView.SingleSelection)
        self.setWrapping(True)
        self.itemDoubleClicked.connect(self._on_double_click)

    def add_photo(self, path: str, thumbnail: QPixmap = None, label: str = ""):
        """添加一张照片到列表"""
        item = QListWidgetItem()
        if thumbnail and not thumbnail.isNull():
            item.setIcon(QIcon(thumbnail))
        item.setText(label or os.path.basename(path))
        item.setToolTip(path)
        item.setSizeHint(QSize(190, 95))
        item.setData(Qt.UserRole, path)
        self.addItem(item)

    def clear_all(self):
        """清空列表"""
        self.clear()

    def _on_double_click(self, item):
        """双击事件处理"""
        path = item.data(Qt.UserRole)
        if path:
            self.item_double_clicked.emit(path)


class PreviewDialog(QDialog):
    """
    照片大图预览对话框

    简洁版：深色背景 + 图片居中 + 关闭按钮。
    支持鼠标滚轮缩放。
    """

    def __init__(self, image_path: str, parent=None):
        super().__init__(parent)
        self.image_path = image_path
        self.setWindowTitle(f"预览 - {os.path.basename(image_path)}")
        self.setMinimumSize(800, 600)
        self.resize(1000, 700)
        self.setStyleSheet("QDialog { background-color: #1a1a2e; }")

        self._scale_factor = 1.0
        self._original_pixmap = None
        self._user_zoomed = False

        layout = QVBoxLayout(self)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.setSpacing(0)

        # 图片标签
        self.label = QLabel()
        self.label.setAlignment(Qt.AlignCenter)
        self.label.setStyleSheet("background-color: #1a1a2e;")
        self.label.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout.addWidget(self.label, 1)

        # 底部工具栏
        toolbar = QFrame()
        toolbar.setStyleSheet("""
            QFrame { background-color: rgba(255,255,255,0.1); border-top: 1px solid rgba(255,255,255,0.1); }
            QPushButton { background-color: rgba(255,255,255,0.15); color: white; border: none;
                          padding: 8px 18px; font-size: 13px; min-width: 80px; }
            QPushButton:hover { background-color: rgba(255,255,255,0.25); }
        """)
        toolbar_layout = QHBoxLayout(toolbar)
        toolbar_layout.setContentsMargins(16, 8, 16, 8)
        toolbar_layout.setSpacing(8)

        btn_zoom_in = QPushButton("放大 (+)")
        btn_zoom_in.clicked.connect(self._zoom_in)
        toolbar_layout.addWidget(btn_zoom_in)

        btn_zoom_out = QPushButton("缩小 (-)")
        btn_zoom_out.clicked.connect(self._zoom_out)
        toolbar_layout.addWidget(btn_zoom_out)

        btn_fit = QPushButton("适应窗口")
        btn_fit.clicked.connect(self._fit_to_window)
        toolbar_layout.addWidget(btn_fit)

        toolbar_layout.addStretch()

        btn_close = QPushButton("关闭")
        btn_close.setStyleSheet(
            "QPushButton { background-color: #ff4757; color: white; border: none;"
            "padding: 8px 18px; font-size: 13px; min-width: 80px; }"
            "QPushButton:hover { background-color: #e8404f; }"
        )
        btn_close.clicked.connect(self.close)
        toolbar_layout.addWidget(btn_close)

        layout.addWidget(toolbar)
        self._load_image()

    def _load_image(self):
        """加载图片并显示"""
        try:
            pixmap = QPixmap(self.image_path)
            if not pixmap.isNull():
                self._original_pixmap = pixmap
                self._fit_to_window()
            else:
                self.label.setText("无法加载图片")
                self.label.setStyleSheet("background-color: #1a1a2e; color: white; font-size: 16px;")
        except Exception:
            self.label.setText("无法加载图片")
            self.label.setStyleSheet("background-color: #1a1a2e; color: white; font-size: 16px;")

    def _update_display(self):
        """根据当前缩放比例更新显示"""
        if not self._original_pixmap or self._original_pixmap.isNull():
            return
        try:
            scaled = self._original_pixmap.scaled(
                int(self._original_pixmap.width() * self._scale_factor),
                int(self._original_pixmap.height() * self._scale_factor),
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation
            )
            self.label.setPixmap(scaled)
        except Exception:
            pass

    def _zoom_in(self):
        """放大图片"""
        self._scale_factor = min(self._scale_factor * 1.25, 5.0)
        self._user_zoomed = True
        self._update_display()

    def _zoom_out(self):
        """缩小图片"""
        self._scale_factor = max(self._scale_factor / 1.25, 0.1)
        self._user_zoomed = True
        self._update_display()

    def _fit_to_window(self):
        """适应窗口大小"""
        self._user_zoomed = False
        if not self._original_pixmap or self._original_pixmap.isNull():
            return
        try:
            label_size = self.label.size() - QSize(20, 20)
            if label_size.width() <= 0 or label_size.height() <= 0:
                return
            scaled = self._original_pixmap.scaled(
                label_size, Qt.KeepAspectRatio, Qt.SmoothTransformation
            )
            self.label.setPixmap(scaled)
            self._scale_factor = scaled.width() / self._original_pixmap.width()
        except Exception:
            pass

    def resizeEvent(self, event):
        """窗口大小改变时重新适应"""
        super().resizeEvent(event)
        if self._original_pixmap and not self._original_pixmap.isNull() and not self._user_zoomed:
            self._fit_to_window()

    def wheelEvent(self, event):
        """鼠标滚轮缩放"""
        if event.angleDelta().y() > 0:
            self._zoom_in()
        else:
            self._zoom_out()
        event.accept()


class AboutDialog(QDialog):
    """关于对话框"""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setWindowTitle("关于")
        self.setFixedSize(400, 300)

        layout = QVBoxLayout(self)
        layout.setContentsMargins(30, 24, 30, 24)
        layout.setSpacing(8)

        title = QLabel(APP_NAME)
        title.setAlignment(Qt.AlignCenter)
        title.setStyleSheet("font-size: 18px; font-weight: bold;")
        layout.addWidget(title)

        version = QLabel(APP_VERSION)
        version.setAlignment(Qt.AlignCenter)
        version.setStyleSheet("color: #666; font-size: 13px;")
        layout.addWidget(version)

        separator = QFrame()
        separator.setFrameShape(QFrame.HLine)
        layout.addWidget(separator)

        info = QLabel(
            "功能特性:\n"
            "  - 自动检测照片缺陷（模糊、闭眼、遮挡等）\n"
            "  - 支持拖拽文件夹快速扫描\n"
            "  - 照片预览与详情查看\n"
            "  - 废片批量移动与报告导出"
        )
        info.setStyleSheet("font-size: 12px; color: #666;")
        layout.addWidget(info)

        layout.addStretch()

        author = QLabel(f"作者: {APP_AUTHOR}")
        author.setAlignment(Qt.AlignCenter)
        author.setStyleSheet("font-size: 12px; color: #666;")
        layout.addWidget(author)

        btn_close = QPushButton("关闭")
        btn_close.setFixedWidth(100)
        btn_close.clicked.connect(self.close)
        layout.addWidget(btn_close, alignment=Qt.AlignCenter)


# ============================================================
# 扫描线程
# ============================================================

class ScanThread(QThread):
    """
    后台扫描线程

    在独立线程中执行照片分类扫描，避免阻塞 UI。
    支持通过标志位中途停止扫描。
    """

    # 信号定义
    progress = pyqtSignal(int, int, str)   # 当前进度, 总数, 当前文件名
    scan_finished = pyqtSignal(dict)       # 扫描结果字典
    error = pyqtSignal(str)                # 错误信息
    stopped = pyqtSignal(int, dict)        # 已扫描数量, 已扫描结果字典

    def __init__(self, image_paths: list, classifier: PhotoClassifier):
        super().__init__()
        self.image_paths = image_paths
        self.classifier = classifier
        self._stop_flag = False

    def run(self):
        """执行扫描任务"""
        try:
            results = {}
            total = len(self.image_paths)

            for i, image_path in enumerate(self.image_paths):
                # 检查停止标志
                if self._stop_flag:
                    # 停止时传递已收集的结果
                    self.stopped.emit(len(results), results)
                    return

                # 发送进度信号
                self.progress.emit(i + 1, total, os.path.basename(image_path))

                # 执行分类检测
                detection_results = self.classifier.classify(image_path)
                results[image_path] = detection_results

            self.scan_finished.emit(results)

        except Exception as e:
            self.error.emit(str(e))

        finally:
            # 线程结束后自动清理
            self.deleteLater()

    def stop(self):
        """请求停止扫描"""
        self._stop_flag = True


# ============================================================
# 主窗口
# ============================================================

class DesktopApp(QMainWindow):
    """
    照片自动分类工具 - 桌面端主窗口（简洁版）

    使用系统 Fusion 风格，标准控件，简洁布局。
    """

    def __init__(self):
        super().__init__()

        # 业务数据
        self.classifier = PhotoClassifier()
        self.scan_results = {}          # 扫描结果 {路径: [DetectionResult]}
        self.current_folder = ""        # 当前选择的文件夹
        self.selected_files = []        # 手动选择的照片文件列表
        self.auto_mode = True           # 自动/手动模式
        self.scan_thread = None         # 扫描线程引用
        self._is_scanning = False       # 是否正在扫描

        # 分批处理相关
        self._batch_timer = None
        self._pending_results = []
        self._batch_index = 0
        self._normal_count = 0
        self._defective_count = 0

        # 窗口状态记忆
        self._settings = QSettings(SETTINGS_ORG, SETTINGS_APP)

        # 初始化 UI
        self._init_ui()
        self._init_menubar()
        self._init_toolbar()
        self._init_statusbar()

        # 恢复窗口状态
        self._restore_window_state()

        # 设置拖拽支持
        self.setAcceptDrops(True)

    # --------------------------------------------------------
    # UI 初始化
    # --------------------------------------------------------

    def _init_ui(self):
        """初始化用户界面布局"""
        self.setWindowTitle(APP_NAME)
        self.setMinimumSize(1200, 800)

        # 中央部件
        central_widget = QWidget()
        self.setCentralWidget(central_widget)
        main_layout = QVBoxLayout(central_widget)
        main_layout.setContentsMargins(6, 6, 6, 6)
        main_layout.setSpacing(6)

        # 1. 文件夹路径 + 进度条区域
        info_layout = QVBoxLayout()
        info_layout.setSpacing(4)

        # 文件夹路径
        path_layout = QHBoxLayout()
        path_layout.setSpacing(6)
        path_label_prefix = QLabel("文件夹路径:")
        path_layout.addWidget(path_label_prefix)

        self.folder_label = QLabel("未选择文件夹（可拖拽文件夹到窗口）")
        self.folder_label.setStyleSheet("color: #666;")
        self.folder_label.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Preferred)
        path_layout.addWidget(self.folder_label, 1)
        info_layout.addLayout(path_layout)

        # 进度条
        self.progress_bar = QProgressBar()
        self.progress_bar.setVisible(False)
        self.progress_bar.setTextVisible(True)
        info_layout.addWidget(self.progress_bar)

        main_layout.addLayout(info_layout)

        # 2. 主内容区域（三栏布局）
        splitter = QSplitter(Qt.Horizontal)

        # 左栏 - 正常照片
        normal_group = QGroupBox("正常照片")
        normal_layout = QVBoxLayout(normal_group)

        self.normal_list = PhotoListWidget()
        self.normal_list.itemClicked.connect(self._on_normal_item_clicked)
        self.normal_list.item_double_clicked.connect(self._on_photo_double_click)
        self.normal_list.setContextMenuPolicy(Qt.CustomContextMenu)
        self.normal_list.customContextMenuRequested.connect(
            lambda pos: self._show_context_menu(pos, is_defective=False)
        )
        normal_layout.addWidget(self.normal_list)

        self.normal_count_label = QLabel("共 0 张")
        self.normal_count_label.setAlignment(Qt.AlignCenter)
        normal_layout.addWidget(self.normal_count_label)

        splitter.addWidget(normal_group)

        # 中栏 - 废片列表
        defective_group = QGroupBox("废片列表")
        defective_layout = QVBoxLayout(defective_group)

        self.defective_list = PhotoListWidget()
        self.defective_list.itemClicked.connect(self._on_defective_item_clicked)
        self.defective_list.item_double_clicked.connect(self._on_photo_double_click)
        self.defective_list.setContextMenuPolicy(Qt.CustomContextMenu)
        self.defective_list.customContextMenuRequested.connect(
            lambda pos: self._show_context_menu(pos, is_defective=True)
        )
        defective_layout.addWidget(self.defective_list)

        self.defective_count_label = QLabel("共 0 张")
        self.defective_count_label.setAlignment(Qt.AlignCenter)
        defective_layout.addWidget(self.defective_count_label)

        splitter.addWidget(defective_group)

        # 右栏 - 照片详情面板
        detail_group = QGroupBox("照片详情")
        detail_layout = QVBoxLayout(detail_group)

        # 预览区域
        self.preview_label = QLabel("选择照片查看详情")
        self.preview_label.setAlignment(Qt.AlignCenter)
        self.preview_label.setMinimumHeight(250)
        self.preview_label.setStyleSheet(
            "background-color: #1a1a2e; color: #888; font-size: 14px;"
        )
        detail_layout.addWidget(self.preview_label)

        # 信息区域
        self.detail_text = QTextEdit()
        self.detail_text.setReadOnly(True)
        self.detail_text.setMaximumHeight(200)
        detail_layout.addWidget(self.detail_text)

        # 底部操作按钮
        action_layout = QHBoxLayout()
        action_layout.setSpacing(8)

        self.btn_view_large = QPushButton("查看大图")
        self.btn_view_large.setEnabled(False)
        self.btn_view_large.clicked.connect(self._view_large_image)
        action_layout.addWidget(self.btn_view_large)

        self.btn_open_location = QPushButton("打开文件位置")
        self.btn_open_location.setEnabled(False)
        self.btn_open_location.clicked.connect(self._open_file_location)
        action_layout.addWidget(self.btn_open_location)

        action_layout.addStretch()
        detail_layout.addLayout(action_layout)

        splitter.addWidget(detail_group)

        splitter.setSizes([400, 400, 300])
        main_layout.addWidget(splitter)

        # 当前选中照片路径
        self._current_photo_path = ""

    def _init_menubar(self):
        """初始化菜单栏"""
        menubar = self.menuBar()

        # 文件菜单
        file_menu = menubar.addMenu("文件")

        action_select = QAction("选择文件夹", self)
        action_select.setShortcut("Ctrl+O")
        action_select.triggered.connect(self._select_folder)
        file_menu.addAction(action_select)

        action_scan = QAction("开始扫描", self)
        action_scan.setShortcut("F5")
        action_scan.triggered.connect(self._toggle_scan)
        file_menu.addAction(action_scan)

        file_menu.addSeparator()

        action_exit = QAction("退出", self)
        action_exit.setShortcut("Ctrl+Q")
        action_exit.triggered.connect(self.close)
        file_menu.addAction(action_exit)

        # 操作菜单
        op_menu = menubar.addMenu("操作")

        action_move = QAction("移动废片", self)
        action_move.triggered.connect(self._move_defective)
        op_menu.addAction(action_move)

        # 帮助菜单
        help_menu = menubar.addMenu("帮助")

        action_about = QAction("关于", self)
        action_about.triggered.connect(self._show_about)
        help_menu.addAction(action_about)

    def _init_toolbar(self):
        """初始化工具栏"""
        toolbar = QToolBar("工具栏")
        toolbar.setMovable(False)
        self.addToolBar(toolbar)

        # 选择文件夹
        action_folder = QAction("选择文件夹", self)
        action_folder.triggered.connect(self._select_folder)
        toolbar.addAction(action_folder)

        # 选择照片文件
        action_photos = QAction("选择照片", self)
        action_photos.triggered.connect(self._select_photos)
        toolbar.addAction(action_photos)

        # 开始/停止扫描
        self.scan_action = QAction("开始扫描", self)
        self.scan_action.triggered.connect(self._toggle_scan)
        toolbar.addAction(self.scan_action)

        toolbar.addSeparator()

        # 模式切换按钮组
        self.auto_action = QAction("自动模式", self)
        self.auto_action.setCheckable(True)
        self.auto_action.setChecked(True)
        self.auto_action.triggered.connect(lambda: self._set_mode(True))
        toolbar.addAction(self.auto_action)

        self.manual_action = QAction("手动确认模式", self)
        self.manual_action.setCheckable(True)
        self.manual_action.triggered.connect(lambda: self._set_mode(False))
        toolbar.addAction(self.manual_action)

        toolbar.addSeparator()

        # 移动废片
        action_move = QAction("移动废片", self)
        action_move.triggered.connect(self._move_defective)
        toolbar.addAction(action_move)

    def _init_statusbar(self):
        """初始化状态栏"""
        self.status_bar = QStatusBar()
        self.setStatusBar(self.status_bar)
        self.status_bar.showMessage("就绪 - 请选择要扫描的文件夹或拖拽文件夹到窗口")

    # --------------------------------------------------------
    # 窗口状态记忆
    # --------------------------------------------------------

    def _restore_window_state(self):
        """从 QSettings 恢复窗口大小和位置"""
        try:
            size = self._settings.value("window_size")
            if size:
                self.resize(size)
            pos = self._settings.value("window_pos")
            if pos:
                self.move(pos)
        except Exception:
            pass

    def _save_window_state(self):
        """保存窗口大小和位置到 QSettings"""
        try:
            self._settings.setValue("window_size", self.size())
            self._settings.setValue("window_pos", self.pos())
        except Exception:
            pass

    def closeEvent(self, event):
        """窗口关闭事件"""
        # 停止分批处理定时器
        if self._batch_timer is not None:
            self._batch_timer.stop()
            self._batch_timer.deleteLater()
            self._batch_timer = None

        # 如果正在扫描，确认退出并等待线程结束
        if self._is_scanning and self.scan_thread and self.scan_thread.isRunning():
            reply = QMessageBox.question(
                self, "确认退出",
                "正在扫描中，确定要退出吗？已扫描的结果将保留。",
                QMessageBox.Yes | QMessageBox.No, QMessageBox.No
            )
            if reply == QMessageBox.No:
                event.ignore()
                return
            self.scan_thread.stop()
            # 带超时等待线程结束，最多等3秒
            if not self.scan_thread.wait(3000):
                self.status_bar.showMessage("警告：扫描线程未能在超时时间内结束")
            self.scan_thread = None

        self._save_window_state()
        event.accept()

    # --------------------------------------------------------
    # 拖拽支持
    # --------------------------------------------------------

    def dragEnterEvent(self, event: QDragEnterEvent):
        """拖入事件处理"""
        if event.mimeData().hasUrls():
            urls = event.mimeData().urls()
            if urls and urls[0].toLocalFile():
                event.acceptProposedAction()
        else:
            event.ignore()

    def dropEvent(self, event: QDropEvent):
        """放下事件处理"""
        urls = event.mimeData().urls()
        if not urls:
            return

        folder_path = urls[0].toLocalFile()
        if not folder_path or not os.path.isdir(folder_path):
            QMessageBox.warning(self, "提示", "请拖入一个文件夹")
            return

        if not is_path_safe(folder_path):
            QMessageBox.warning(self, "安全警告", "该路径不安全，禁止访问")
            return

        self.current_folder = folder_path
        self.folder_label.setText(folder_path)
        self.scan_action.setEnabled(True)
        self.status_bar.showMessage(f"已选择文件夹: {folder_path}")

    # --------------------------------------------------------
    # 文件夹选择与扫描
    # --------------------------------------------------------

    def _select_folder(self):
        """选择文件夹对话框"""
        folder = QFileDialog.getExistingDirectory(self, "选择照片文件夹")
        if not folder:
            return

        if not is_path_safe(folder):
            QMessageBox.warning(self, "安全警告", "该路径不安全，禁止访问")
            return

        self.current_folder = folder
        self.folder_label.setText(folder)
        self.scan_action.setEnabled(True)
        self.status_bar.showMessage(f"已选择文件夹: {folder}")

    def _select_photos(self):
        """选择照片文件对话框（支持多选）"""
        files, _ = QFileDialog.getOpenFileNames(
            self, "选择照片文件",
            "",
            "Images (*.jpg *.jpeg *.png *.bmp *.gif *.webp *.tiff *.tif)"
        )
        if not files:
            return

        # 安全检查
        for f in files:
            if not is_path_safe(f):
                QMessageBox.warning(self, "安全警告", f"路径不安全，禁止访问:\n{mask_path(f)}")
                return

        # 更新 current_folder 为第一个文件所在目录
        self.current_folder = os.path.dirname(files[0])
        self.selected_files = files

        # 更新路径显示
        if len(files) == 1:
            display_text = files[0]
        else:
            display_text = f"{os.path.basename(files[0])} 等 {len(files)} 个文件"
        self.folder_label.setText(display_text)
        self.scan_action.setEnabled(True)
        self.status_bar.showMessage(f"已选择 {len(files)} 个照片文件")

    def _toggle_scan(self):
        """切换扫描状态（开始/停止）"""
        if self._is_scanning:
            self._stop_scan()
        else:
            self._start_scan()

    def _start_scan(self):
        """开始扫描"""
        if not self.current_folder:
            QMessageBox.warning(self, "提示", "请先选择文件夹")
            return

        if self._is_scanning and self.scan_thread and self.scan_thread.isRunning():
            QMessageBox.warning(self, "提示", "正在扫描中，请等待完成或点击停止")
            return

        # 清空现有结果
        self.normal_list.clear_all()
        self.defective_list.clear_all()
        self.scan_results = {}
        self.detail_text.clear()
        self._current_photo_path = ""
        self.btn_view_large.setEnabled(False)
        self.btn_open_location.setEnabled(False)

        # 重置预览区域
        self.preview_label.clear()
        self.preview_label.setText("选择照片查看详情")
        self.preview_label.setStyleSheet(
            "background-color: #1a1a2e; color: #888; font-size: 14px;"
        )

        # 重置计数标签
        self.normal_count_label.setText("共 0 张")
        self.defective_count_label.setText("共 0 张")

        # 扫描图片文件
        try:
            if self.selected_files:
                image_files = list(self.selected_files)
                self.selected_files = []  # 扫描前清空
            else:
                image_files = PhotoClassifier.scan_directory(self.current_folder)
                image_files = list(dict.fromkeys(image_files))  # 去重保序
        except Exception as e:
            QMessageBox.critical(self, "扫描出错", f"扫描目录时出错: {str(e)}")
            return

        if not image_files:
            QMessageBox.information(self, "提示", "在所选文件夹中未找到图片文件")
            return

        # 显示进度条
        self.progress_bar.setVisible(True)
        self.progress_bar.setValue(0)
        self.progress_bar.setMaximum(len(image_files))

        # 切换按钮状态
        self._is_scanning = True
        self.scan_action.setText("停止扫描")
        self.scan_action.setEnabled(True)

        self.status_bar.showMessage(f"正在扫描 {len(image_files)} 张图片...")

        # 启动后台扫描线程
        self.scan_thread = ScanThread(image_files, self.classifier)
        self.scan_thread.progress.connect(self._on_scan_progress)
        self.scan_thread.scan_finished.connect(self._on_scan_finished)
        self.scan_thread.error.connect(self._on_scan_error)
        self.scan_thread.stopped.connect(self._on_scan_stopped)
        self.scan_thread.start()

    def _stop_scan(self):
        """停止扫描"""
        if self.scan_thread and self.scan_thread.isRunning():
            self.scan_thread.stop()
            self.status_bar.showMessage("正在停止扫描...")

    def _on_scan_progress(self, current: int, total: int, filename: str):
        """扫描进度回调"""
        self.progress_bar.setValue(current)
        percent = int(current / total * 100) if total > 0 else 0
        self.status_bar.showMessage(f"正在扫描: {current}/{total} ({percent}%) - {filename}")

    def _on_scan_finished(self, results: dict):
        """
        扫描完成回调 - 使用 QTimer 分批处理结果

        避免在主线程中同步为每张图片生成缩略图导致内存峰值和界面冻结。
        """
        self.scan_results = results
        self._reset_scan_ui()

        # 准备分批处理
        self._pending_results = list(results.items())
        self._batch_index = 0
        self._normal_count = 0
        self._defective_count = 0

        # 每50ms处理一批（每批5张），让主线程有机会处理事件
        self._batch_timer = QTimer(self)
        self._batch_timer.timeout.connect(self._process_next_batch)
        self._batch_timer.start(BATCH_INTERVAL_MS)

    def _process_next_batch(self):
        """分批处理扫描结果，避免主线程长时间冻结"""
        end = min(self._batch_index + BATCH_SIZE, len(self._pending_results))

        for i in range(self._batch_index, end):
            path, detections = self._pending_results[i]
            defects = [d for d in detections if d.is_defective]

            # 生成缩略图（带内存保护）
            thumbnail = self._generate_thumbnail(path)

            if defects:
                self._defective_count += 1
                main_defect = max(defects, key=lambda d: d.confidence)
                dtype_name = DEFECT_TYPE_NAMES.get(
                    main_defect.defect_type, "未知"
                ) if main_defect.defect_type else "未知"
                label = f"[{dtype_name}] {os.path.basename(path)}"
                self.defective_list.add_photo(path, thumbnail, label)
            else:
                self._normal_count += 1
                self.normal_list.add_photo(path, thumbnail)

        self._batch_index = end
        total = len(self._pending_results)

        # 更新状态栏显示进度
        self.status_bar.showMessage(f"正在加载结果: {self._batch_index}/{total}...")

        if self._batch_index >= total:
            # 全部处理完成
            self._batch_timer.stop()
            self._batch_timer.deleteLater()
            self._batch_timer = None
            self._update_final_stats()

    def _update_final_stats(self):
        """所有结果加载完毕后更新统计"""
        total = self._normal_count + self._defective_count
        self.normal_count_label.setText(f"共 {self._normal_count} 张")
        self.defective_count_label.setText(f"共 {self._defective_count} 张")
        self.status_bar.showMessage(
            f"扫描完成 - 共 {total} 张, 正常 {self._normal_count} 张, 废片 {self._defective_count} 张"
        )

    def _on_scan_stopped(self, scanned_count: int, partial_results: dict):
        """
        扫描被停止回调 - 使用已扫描的部分结果

        参数:
            scanned_count: 已扫描的图片数量
            partial_results: 已扫描的结果字典
        """
        self.scan_results = partial_results
        self._reset_scan_ui()

        # 使用分批处理加载部分结果
        self._pending_results = list(partial_results.items())
        self._batch_index = 0
        self._normal_count = 0
        self._defective_count = 0

        self._batch_timer = QTimer(self)
        self._batch_timer.timeout.connect(self._process_next_batch_stopped)
        self._batch_timer.start(BATCH_INTERVAL_MS)

    def _process_next_batch_stopped(self):
        """分批处理停止后的部分结果"""
        end = min(self._batch_index + BATCH_SIZE, len(self._pending_results))

        for i in range(self._batch_index, end):
            path, detections = self._pending_results[i]
            defects = [d for d in detections if d.is_defective]

            thumbnail = self._generate_thumbnail(path)

            if defects:
                self._defective_count += 1
                main_defect = max(defects, key=lambda d: d.confidence)
                dtype_name = DEFECT_TYPE_NAMES.get(
                    main_defect.defect_type, "未知"
                ) if main_defect.defect_type else "未知"
                label = f"[{dtype_name}] {os.path.basename(path)}"
                self.defective_list.add_photo(path, thumbnail, label)
            else:
                self._normal_count += 1
                self.normal_list.add_photo(path, thumbnail)

        self._batch_index = end
        total = len(self._pending_results)

        self.status_bar.showMessage(f"正在加载结果: {self._batch_index}/{total}...")

        if self._batch_index >= total:
            self._batch_timer.stop()
            self._batch_timer.deleteLater()
            self._batch_timer = None

            total_count = self._normal_count + self._defective_count
            self.normal_count_label.setText(f"共 {self._normal_count} 张")
            self.defective_count_label.setText(f"共 {self._defective_count} 张")
            self.status_bar.showMessage(
                f"扫描已停止 - 已扫描 {total_count} 张, "
                f"正常 {self._normal_count} 张, 废片 {self._defective_count} 张"
            )

    def _on_scan_error(self, error_msg: str):
        """扫描出错回调"""
        self._reset_scan_ui()
        QMessageBox.critical(self, "扫描出错", f"扫描过程中出错:\n{error_msg}")
        self.status_bar.showMessage("扫描出错，请检查文件或重试")

    def _reset_scan_ui(self):
        """重置扫描相关 UI 状态"""
        self._is_scanning = False
        self.progress_bar.setVisible(False)
        self.progress_bar.setValue(0)
        self.scan_action.setText("开始扫描")
        self.scan_action.setEnabled(True)

    # --------------------------------------------------------
    # 照片列表交互
    # --------------------------------------------------------

    def _on_normal_item_clicked(self, item):
        """正常照片被点击"""
        path = item.data(Qt.UserRole)
        if path:
            self._show_photo_detail(path, [])

    def _on_defective_item_clicked(self, item):
        """废片被点击"""
        path = item.data(Qt.UserRole)
        if path and path in self.scan_results:
            defects = [d for d in self.scan_results[path] if d.is_defective]
            self._show_photo_detail(path, defects)

    def _on_photo_double_click(self, path: str):
        """照片双击事件 - 打开文件位置"""
        self._open_file_location_by_path(path)

    def _show_photo_detail(self, path: str, defects: list):
        """显示照片详情"""
        self._current_photo_path = path
        self.btn_view_large.setEnabled(True)
        self.btn_open_location.setEnabled(True)

        # 显示预览图
        try:
            pixmap = QPixmap(path)
            if not pixmap.isNull():
                scaled = pixmap.scaled(
                    self.preview_label.size(),
                    Qt.KeepAspectRatio,
                    Qt.SmoothTransformation
                )
                self.preview_label.setPixmap(scaled)
                self.preview_label.setStyleSheet("background-color: #1a1a2e;")
            else:
                self.preview_label.setText("无法加载预览")
                self.preview_label.setStyleSheet(
                    "background-color: #1a1a2e; color: #ff4757; font-size: 14px;"
                )
        except Exception:
            self.preview_label.setText("无法加载预览")
            self.preview_label.setStyleSheet(
                "background-color: #1a1a2e; color: #ff4757; font-size: 14px;"
            )

        # 构建详细信息
        info_lines = []
        info_lines.append(f"<b>文件名:</b> {os.path.basename(path)}")

        try:
            file_size = os.path.getsize(path) if os.path.exists(path) else 0
            if file_size >= 1024 * 1024:
                size_str = f"{file_size / (1024 * 1024):.2f} MB"
            else:
                size_str = f"{file_size / 1024:.1f} KB"
            info_lines.append(f"<b>文件大小:</b> {size_str}")
        except Exception:
            info_lines.append("<b>文件大小:</b> 未知")

        if defects:
            info_lines.append(f"<b>状态:</b> <span style='color:red;'>废片</span>")
            info_lines.append(f"<b>缺陷数量:</b> {len(defects)}")

            main_defect = max(defects, key=lambda d: d.confidence)
            dtype_name = DEFECT_TYPE_NAMES.get(
                main_defect.defect_type, "未知"
            ) if main_defect.defect_type else "未知"
            info_lines.append(f"<b>主要缺陷类型:</b> <span style='color:red;'>{dtype_name}</span>")
            info_lines.append(f"<b>置信度:</b> {main_defect.confidence * 100:.0f}%")

            info_lines.append("")
            info_lines.append("<b>缺陷详情:</b>")
            for i, d in enumerate(defects, 1):
                d_name = DEFECT_TYPE_NAMES.get(d.defect_type, "未知") if d.defect_type else "未知"
                info_lines.append(f"  {i}. {d_name} ({d.confidence * 100:.0f}%) - {d.description}")
        else:
            info_lines.append(f"<b>状态:</b> <span style='color:green;'>正常</span>")
            info_lines.append("<b>缺陷类型:</b> 无")
            info_lines.append("<b>描述:</b> 未检测到缺陷")

        self.detail_text.setHtml("<br>".join(info_lines))

    def _generate_thumbnail(self, image_path: str, max_size: int = THUMBNAIL_MAX_SIZE) -> QPixmap:
        """
        生成缩略图（带内存保护）

        加载后立即缩小，不保留原始 pixmap 引用。

        参数:
            image_path: 图片文件路径
            max_size: 缩略图最大尺寸（像素）

        返回:
            QPixmap 缩略图，加载失败返回空 QPixmap
        """
        try:
            pixmap = QPixmap(image_path)
            if not pixmap.isNull():
                # 立即缩小，释放原始大图内存
                thumbnail = pixmap.scaled(
                    max_size, max_size,
                    Qt.KeepAspectRatio,
                    Qt.SmoothTransformation
                )
                # 不保留原始 pixmap 引用，让 GC 回收
                return thumbnail
        except Exception:
            pass
        return QPixmap()

    # --------------------------------------------------------
    # 右键菜单
    # --------------------------------------------------------

    def _show_context_menu(self, pos: QPoint, is_defective: bool = False):
        """显示右键上下文菜单"""
        list_widget = self.defective_list if is_defective else self.normal_list
        item = list_widget.itemAt(pos)
        if not item:
            return

        path = item.data(Qt.UserRole)
        if not path:
            return

        menu = QMenu(self)

        action_preview = menu.addAction("查看大图")
        action_preview.triggered.connect(lambda: self._view_large_image_by_path(path))

        action_open = menu.addAction("打开文件位置")
        action_open.triggered.connect(lambda: self._open_file_location_by_path(path))

        action_copy = menu.addAction("复制路径")
        action_copy.triggered.connect(lambda: self._copy_path_to_clipboard(path))

        if is_defective:
            menu.addSeparator()
            action_move_single = menu.addAction("单独移动此废片")
            action_move_single.triggered.connect(lambda: self._move_single_defective(path))

        menu.exec_(list_widget.mapToGlobal(pos))

    def _view_large_image(self):
        """查看当前选中照片的大图"""
        if self._current_photo_path:
            self._view_large_image_by_path(self._current_photo_path)

    def _view_large_image_by_path(self, path: str):
        """查看指定路径照片的大图"""
        if not path or not os.path.exists(path):
            QMessageBox.warning(self, "提示", "文件不存在或路径无效")
            return
        dialog = PreviewDialog(path, self)
        dialog.exec_()

    def _open_file_location(self):
        """打开当前选中照片的文件位置"""
        if self._current_photo_path:
            self._open_file_location_by_path(self._current_photo_path)

    def _open_file_location_by_path(self, path: str):
        """用系统文件管理器打开文件所在文件夹并选中文件"""
        if not path or not os.path.exists(path):
            QMessageBox.warning(self, "提示", "文件不存在或路径无效")
            return

        try:
            if sys.platform == "win32":
                subprocess.Popen(f'explorer /select,"{os.path.normpath(path)}"')
            elif sys.platform == "darwin":
                subprocess.Popen(["open", "-R", path])
            else:
                subprocess.Popen(["xdg-open", os.path.dirname(path)])
        except Exception as e:
            QMessageBox.warning(self, "提示", f"无法打开文件位置: {str(e)}")

    def _copy_path_to_clipboard(self, path: str):
        """复制文件路径到剪贴板"""
        try:
            clipboard = QApplication.clipboard()
            clipboard.setText(path)
            self.status_bar.showMessage("路径已复制到剪贴板", 3000)
        except Exception:
            pass

    def _move_single_defective(self, path: str):
        """单独移动一张废片"""
        if not path or path not in self.scan_results:
            return

        defects = [d for d in self.scan_results[path] if d.is_defective]
        if not defects:
            return

        reply = QMessageBox.question(
            self, "确认移动",
            f"确定要将此废片移动到\n"
            f"'{self.current_folder}/废片/' 文件夹吗？\n\n"
            f"文件: {os.path.basename(path)}",
            QMessageBox.Yes | QMessageBox.No, QMessageBox.No
        )

        if reply == QMessageBox.Yes:
            try:
                defective_images = {path: defects}
                moved = self.classifier.move_defective(
                    defective_images, self.current_folder, mode="move"
                )
                if moved:
                    QMessageBox.information(
                        self, "完成",
                        f"已移动 1 张废片到 '{self.current_folder}/废片/' 文件夹"
                    )
                    self.status_bar.showMessage("已移动废片")
                else:
                    QMessageBox.warning(self, "失败", "移动文件失败")
            except Exception as e:
                QMessageBox.critical(
                    self, "移动失败",
                    f"移动文件时出错:\n{str(e)}"
                )

    # --------------------------------------------------------
    # 模式切换
    # --------------------------------------------------------

    def _set_mode(self, auto: bool):
        """设置自动/手动模式"""
        self.auto_mode = auto
        self.auto_action.setChecked(auto)
        self.manual_action.setChecked(not auto)
        mode_text = "自动模式" if auto else "手动确认模式"
        self.status_bar.showMessage(f"已切换到{mode_text}")

    # --------------------------------------------------------
    # 移动废片
    # --------------------------------------------------------

    def _move_defective(self):
        """移动废片到废片文件夹（带安全确认）"""
        if not self.scan_results:
            QMessageBox.information(self, "提示", "请先扫描照片")
            return

        # 构建废片字典
        defective_images = {}
        for path, results in self.scan_results.items():
            defects = [r for r in results if r.is_defective]
            if defects:
                defective_images[path] = defects

        if not defective_images:
            QMessageBox.information(self, "提示", "没有检测到废片")
            return

        count = len(defective_images)
        target_path = os.path.join(self.current_folder, "废片")

        reply = QMessageBox.question(
            self, "确认移动",
            f"即将移动 {count} 张废片到以下位置:\n\n"
            f"{target_path}\n\n"
            f"此操作将移动文件到按缺陷类型分类的子文件夹中。\n"
            f"确定要继续吗？",
            QMessageBox.Yes | QMessageBox.No, QMessageBox.No
        )

        if reply == QMessageBox.Yes:
            try:
                moved = self.classifier.move_defective(
                    defective_images, self.current_folder, mode="move"
                )
                QMessageBox.information(
                    self, "完成",
                    f"已成功移动 {len(moved)} 张废片到 '{target_path}' 文件夹"
                )
                self.status_bar.showMessage(f"已移动 {len(moved)} 张废片")
            except Exception as e:
                QMessageBox.critical(
                    self, "移动失败",
                    f"移动废片时出错:\n{str(e)}"
                )

    # --------------------------------------------------------
    # 关于对话框
    # --------------------------------------------------------

    def _show_about(self):
        """显示关于对话框"""
        dialog = AboutDialog(self)
        dialog.exec_()


# ============================================================
# 应用入口
# ============================================================

def run():
    """启动桌面应用"""
    app = QApplication(sys.argv)

    # 使用 Fusion 风格（跨平台一致的简洁外观）
    app.setStyle("Fusion")

    # 设置全局字体
    font = QFont("Microsoft YaHei", 10)
    app.setFont(font)

    # 创建并显示主窗口
    window = DesktopApp()
    window.show()

    sys.exit(app.exec_())


if __name__ == "__main__":
    run()

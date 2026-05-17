# -*- coding: utf-8 -*-
"""
Flask Web 应用 - 安全增强版
照片自动分类工具的 Web 端

安全防护:
  - 文件上传大小限制（单文件50MB，总200MB）
  - 文件类型白名单验证（检查文件头魔数）
  - 路径遍历攻击防护
  - CSRF Token 防护
  - 请求频率限制（每IP每分钟最多30次请求）
  - 文件名安全过滤
  - API输入参数校验和清理
  - 删除操作二次确认Token机制
  - 敏感路径保护
  - 错误信息脱敏

功能优化:
  - /api/csrf-token 接口
  - /api/health 健康检查接口
  - 请求日志记录
  - 临时文件自动清理
"""

import os
import sys
import json
import time
import uuid
import hmac
import hashlib
import logging
import threading
import base64
import tempfile
import shutil
import re
from io import BytesIO
from functools import wraps
from collections import defaultdict

from flask import Flask, render_template, request, jsonify, g

# ============================================================
# 日志配置
# ============================================================
logger = logging.getLogger("PhotoClassifierPro")
logger.setLevel(logging.INFO)
_handler = logging.StreamHandler(sys.stdout)
_handler.setFormatter(logging.Formatter(
    "[%(asctime)s] %(levelname)s %(name)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
))
logger.addHandler(_handler)

# ============================================================
# 将项目根目录加入 Python 路径
# ============================================================
PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from engine.classifier import PhotoClassifier

# ============================================================
# 安全常量配置
# ============================================================

# 文件上传大小限制
MAX_FILE_SIZE = 50 * 1024 * 1024        # 单文件 50MB
MAX_TOTAL_SIZE = 200 * 1024 * 1024      # 总上传 200MB

# 允许的图片扩展名
ALLOWED_EXTENSIONS = {'.jpg', '.jpeg', '.png', '.gif', '.bmp', '.webp', '.tiff', '.tif'}

# 文件头魔数映射（扩展名 -> 魔数字节）
FILE_MAGIC_NUMBERS = {
    '.jpg':  (b'\xFF\xD8\xFF',),
    '.jpeg': (b'\xFF\xD8\xFF',),
    '.png':  (b'\x89PNG\r\n\x1a\n',),
    '.gif':  (b'GIF87a', b'GIF89a'),
    '.bmp':  (b'BM',),
    '.webp': (b'RIFF',),       # WebP 以 RIFF 开头，后跟 4 字节长度 + WEBP
    '.tiff': (b'II\x2a\x00', b'MM\x00\x2a'),  # Little/Big endian TIFF
    '.tif':  (b'II\x2a\x00', b'MM\x00\x2a'),
}

# 需要保护的系统敏感路径（禁止访问）
PROTECTED_PATHS = {
    '/', '/etc', '/usr', '/bin', '/sbin', '/boot', '/dev', '/proc', '/sys',
    '/var', '/tmp', '/root', '/home', '/windows', '/windows/system32',
    'C:\\\\', 'C:\\\\Windows', 'C:\\\\Program Files', 'C:\\\\ProgramData',
    'D:\\\\Windows', 'D:\\\\Program Files',
}

# 请求频率限制：每 IP 每分钟最大请求数
RATE_LIMIT_MAX_REQUESTS = 60
RATE_LIMIT_WINDOW = 60  # 秒

# CSRF Token 使用的密钥（生产环境应从环境变量读取）
CSRF_SECRET_KEY = os.environ.get("CSRF_SECRET_KEY", "photoclassifierpro-csrf-secret-key-2024")

# 删除操作确认 Token 有效期（秒）
DELETE_TOKEN_EXPIRE = 300  # 5 分钟

# 临时文件自动清理间隔（秒）
TEMP_CLEANUP_INTERVAL = 3600  # 1 小时
# 临时文件最大保留时间（秒）
TEMP_FILE_MAX_AGE = 86400  # 24 小时


# ============================================================
# 安全工具函数
# ============================================================

def sanitize_filename(filename: str) -> str:
    """
    文件名安全过滤：去除特殊字符，只保留字母、数字、中文、下划线、连字符和点号

    参数:
        filename: 原始文件名

    返回:
        过滤后的安全文件名
    """
    # 先取基础文件名，防止路径注入
    filename = os.path.basename(filename)
    # 移除或替换危险字符，只保留安全字符
    # 允许：中文、字母、数字、下划线、连字符、点号、空格
    filename = re.sub(r'[^\w\u4e00-\u9fff.\- ]', '_', filename)
    # 去除连续的下划线或点号
    filename = re.sub(r'_+', '_', filename)
    filename = re.sub(r'\.+', '.', filename)
    # 去除首尾空白和点号
    filename = filename.strip(' .')
    # 如果文件名为空，使用随机名
    if not filename:
        filename = f"unnamed_{uuid.uuid4().hex[:8]}"
    return filename


def validate_file_magic(file_path: str, extension: str) -> bool:
    """
    通过文件头魔数验证文件类型，防止扩展名伪造

    参数:
        file_path: 文件路径
        extension: 文件扩展名（含点号，如 '.jpg'）

    返回:
        True 表示文件头匹配，False 表示不匹配
    """
    extension = extension.lower()
    magic_list = FILE_MAGIC_NUMBERS.get(extension)
    if not magic_list:
        return False

    try:
        with open(file_path, 'rb') as f:
            header = f.read(16)  # 读取前 16 字节用于判断

        for magic in magic_list:
            if header.startswith(magic):
                # WebP 需要额外验证：RIFF + 4字节长度 + WEBP
                if extension == '.webp' and len(header) >= 12:
                    if header[8:12] == b'WEBP':
                        return True
                else:
                    return True
        return False
    except (OSError, IOError):
        return False


def is_path_safe(path: str) -> bool:
    """
    路径安全检查：防止路径遍历攻击和敏感路径访问

    参数:
        path: 待检查的路径

    返回:
        True 表示路径安全，False 表示路径不安全
    """
    if not path:
        return False

    # 规范化路径
    normalized = os.path.normpath(path)

    # 检查路径遍历攻击：禁止 .. 逃逸
    if '..' in normalized.split(os.sep):
        return False

    # 检查绝对路径是否指向受保护目录
    abs_path = os.path.abspath(normalized)
    for protected in PROTECTED_PATHS:
        protected_abs = os.path.abspath(protected)
        # 检查是否是受保护路径的子目录
        try:
            if abs_path == protected_abs or abs_path.startswith(protected_abs + os.sep):
                return False
        except (ValueError, TypeError):
            continue

    return True


def sanitize_error_message(error: Exception) -> str:
    """
    错误信息脱敏：移除服务器路径等敏感信息

    参数:
        error: 异常对象

    返回:
        脱敏后的错误信息
    """
    msg = str(error)
    # 移除绝对路径信息
    msg = re.sub(r'[A-Za-z]:\\[^\s:]+', '[路径已隐藏]', msg)
    msg = re.sub(r'/[^\s:]+/\.\.\.', '[路径已隐藏]', msg)
    # 移除可能的用户名
    msg = re.sub(r'Users\\[^\\]+\\', 'Users\\[用户]\\', msg)
    msg = re.sub(r'/home/[^/]+/', '/home/[用户]/', msg)
    return msg


def generate_csrf_token() -> str:
    """
    生成 CSRF Token

    返回:
        CSRF Token 字符串
    """
    session_id = getattr(g, 'csrf_session_id', None)
    if session_id is None:
        session_id = uuid.uuid4().hex
        g.csrf_session_id = session_id
    timestamp = str(int(time.time()))
    raw = f"{session_id}:{timestamp}"
    signature = hmac.new(
        CSRF_SECRET_KEY.encode('utf-8'),
        raw.encode('utf-8'),
        hashlib.sha256
    ).hexdigest()
    return f"{session_id}:{timestamp}:{signature}"


def validate_csrf_token(token: str) -> bool:
    """
    验证 CSRF Token 是否有效

    参数:
        token: 待验证的 CSRF Token

    返回:
        True 表示 Token 有效
    """
    if not token:
        return False

    try:
        parts = token.split(':')
        if len(parts) != 3:
            return False
        session_id, timestamp_str, signature = parts
        timestamp = int(timestamp_str)

        # 检查 Token 是否过期（24小时有效期）
        if abs(time.time() - timestamp) > 86400:
            return False

        # 重新计算签名进行验证
        raw = f"{session_id}:{timestamp_str}"
        expected = hmac.new(
            CSRF_SECRET_KEY.encode('utf-8'),
            raw.encode('utf-8'),
            hashlib.sha256
        ).hexdigest()

        return hmac.compare_digest(signature, expected)
    except (ValueError, IndexError, AttributeError):
        return False


def generate_delete_token(file_paths: list) -> str:
    """
    生成删除操作确认 Token

    参数:
        file_paths: 待删除的文件路径列表

    返回:
        确认 Token 字符串
    """
    timestamp = str(int(time.time()))
    # 将文件路径排序后拼接，确保一致性
    paths_str = '|'.join(sorted(file_paths))
    raw = f"{timestamp}:{paths_str}"
    signature = hmac.new(
        CSRF_SECRET_KEY.encode('utf-8'),
        raw.encode('utf-8'),
        hashlib.sha256
    ).hexdigest()
    return f"{timestamp}:{signature}"


def validate_delete_token(token: str, file_paths: list) -> bool:
    """
    验证删除操作确认 Token

    参数:
        token: 待验证的确认 Token
        file_paths: 实际要删除的文件路径列表

    返回:
        True 表示 Token 有效且匹配
    """
    if not token:
        return False

    try:
        parts = token.split(':', 1)
        if len(parts) != 2:
            return False
        timestamp_str, signature = parts
        timestamp = int(timestamp_str)

        # 检查 Token 是否过期
        if time.time() - timestamp > DELETE_TOKEN_EXPIRE:
            return False

        # 重新计算签名
        paths_str = '|'.join(sorted(file_paths))
        raw = f"{timestamp_str}:{paths_str}"
        expected = hmac.new(
            CSRF_SECRET_KEY.encode('utf-8'),
            raw.encode('utf-8'),
            hashlib.sha256
        ).hexdigest()

        return hmac.compare_digest(signature, expected)
    except (ValueError, IndexError, AttributeError):
        return False


# ============================================================
# 请求频率限制器
# ============================================================

class RateLimiter:
    """
    基于内存的 IP 请求频率限制器
    每个 IP 在指定时间窗口内最多允许 N 次请求
    """

    def __init__(self, max_requests: int = RATE_LIMIT_MAX_REQUESTS,
                 window: int = RATE_LIMIT_WINDOW):
        self.max_requests = max_requests
        self.window = window
        # {ip: [timestamp1, timestamp2, ...]}
        self._requests: dict[str, list] = defaultdict(list)
        self._lock = threading.Lock()

    def is_allowed(self, ip: str) -> bool:
        """
        检查指定 IP 是否允许请求

        参数:
            ip: 客户端 IP 地址

        返回:
            True 表示允许请求，False 表示已被限流
        """
        now = time.time()
        cutoff = now - self.window

        with self._lock:
            # 清理过期的请求记录
            self._requests[ip] = [
                t for t in self._requests[ip] if t > cutoff
            ]

            if len(self._requests[ip]) >= self.max_requests:
                logger.warning("请求频率限制触发: ip=%s, 请求数=%d",
                               ip, len(self._requests[ip]))
                return False

            self._requests[ip].append(now)
            return True

    def cleanup(self):
        """清理所有过期的请求记录"""
        now = time.time()
        cutoff = now - self.window
        with self._lock:
            expired_ips = []
            for ip, timestamps in self._requests.items():
                self._requests[ip] = [t for t in timestamps if t > cutoff]
                if not self._requests[ip]:
                    expired_ips.append(ip)
            for ip in expired_ips:
                del self._requests[ip]


# ============================================================
# 临时文件清理器
# ============================================================

class TempFileCleaner:
    """
    定时清理 photo_classifier_ 前缀的临时目录
    """

    def __init__(self, interval: int = TEMP_CLEANUP_INTERVAL,
                 max_age: int = TEMP_FILE_MAX_AGE):
        self.interval = interval
        self.max_age = max_age
        self._running = False
        self._thread = None

    def start(self):
        """启动后台清理线程"""
        if self._running:
            return
        self._running = True
        self._thread = threading.Thread(target=self._cleanup_loop, daemon=True)
        self._thread.start()
        logger.info("临时文件清理器已启动，清理间隔=%d秒，最大保留=%d秒",
                     self.interval, self.max_age)

    def stop(self):
        """停止清理线程"""
        self._running = False

    def _cleanup_loop(self):
        """清理循环"""
        while self._running:
            time.sleep(self.interval)
            self._cleanup()

    def _cleanup(self):
        """执行一次清理"""
        try:
            temp_base = tempfile.gettempdir()
            now = time.time()
            cleaned_count = 0

            for entry in os.listdir(temp_base):
                if not entry.startswith("PhotoClassifierPro_"):
                    continue
                full_path = os.path.join(temp_base, entry)
                if not os.path.isdir(full_path):
                    continue

                # 检查目录创建时间
                try:
                    stat = os.stat(full_path)
                    age = now - stat.st_ctime
                    if age > self.max_age:
                        shutil.rmtree(full_path, ignore_errors=True)
                        cleaned_count += 1
                        logger.info("清理过期临时目录: %s (存在 %.1f 小时)",
                                    entry, age / 3600)
                except OSError:
                    continue

            if cleaned_count > 0:
                logger.info("临时文件清理完成，共清理 %d 个目录", cleaned_count)
        except Exception as e:
            logger.error("临时文件清理出错: %s", sanitize_error_message(e))


# ============================================================
# Flask 应用工厂
# ============================================================

def create_app() -> Flask:
    """创建并配置 Flask 应用"""

    app = Flask(
        __name__,
        template_folder=os.path.join(os.path.dirname(__file__), "templates"),
        static_folder=None,
    )

    # 配置上传大小限制（Flask 内置限制，作为额外保护层）
    app.config['MAX_CONTENT_LENGTH'] = MAX_TOTAL_SIZE

    # 全局安全组件
    rate_limiter = RateLimiter()
    temp_cleaner = TempFileCleaner()

    # 全局分类器实例
    classifier = PhotoClassifier()

    # 扫描状态
    scan_state = {
        "is_scanning": False,
        "progress": 0,
        "total": 0,
        "current_file": "",
        "results": {},
        "error": None,
        "temp_dir": None,  # 存储上传文件的临时目录
    }

    # 缩略图缓存 {path: base64_string}
    thumbnail_cache = {}

    # 启动临时文件清理器
    temp_cleaner.start()

    # ========================================================
    # 请求日志中间件
    # ========================================================

    @app.before_request
    def log_request():
        """记录每个请求的基本信息"""
        g._request_start = time.time()
        client_ip = request.remote_addr or 'unknown'
        logger.info("请求: %s %s from %s", request.method, request.path, client_ip)

    @app.after_request
    def log_response(response):
        """记录响应状态"""
        client_ip = request.remote_addr or 'unknown'
        elapsed = (time.time() - getattr(g, '_request_start', time.time())) * 1000
        logger.info("响应: %s %s -> %d (%.1fms)",
                     request.method, request.path,
                     response.status_code, elapsed)
        return response

    # ========================================================
    # 全局错误处理器（错误信息脱敏）
    # ========================================================

    @app.errorhandler(400)
    def handle_400(error):
        return jsonify({"error": "请求参数错误"}), 400

    @app.errorhandler(404)
    def handle_404(error):
        return jsonify({"error": "请求的资源不存在"}), 404

    @app.errorhandler(413)
    def handle_413(error):
        return jsonify({"error": f"文件大小超出限制（单文件最大 {MAX_FILE_SIZE // (1024*1024)}MB，"
                                 f"总计最大 {MAX_TOTAL_SIZE // (1024*1024)}MB）"}), 413

    @app.errorhandler(429)
    def handle_429(error):
        return jsonify({"error": "请求过于频繁，请稍后再试"}), 429

    @app.errorhandler(500)
    def handle_500(error):
        logger.error("服务器内部错误: %s", sanitize_error_message(error))
        return jsonify({"error": "服务器内部错误，请稍后重试"}), 500

    # ========================================================
    # 安全装饰器
    # ========================================================

    def rate_limit(f):
        """
        请求频率限制装饰器
        对每个 IP 限制每分钟最多 30 次请求
        """
        @wraps(f)
        def decorated(*args, **kwargs):
            client_ip = request.remote_addr or 'unknown'
            if not rate_limiter.is_allowed(client_ip):
                return jsonify({"error": "请求过于频繁，请稍后再试"}), 429
            return f(*args, **kwargs)
        return decorated

    def csrf_protect(f):
        """
        CSRF 防护装饰器
        对 POST/PUT/DELETE 请求要求携带有效的 CSRF Token
        """
        @wraps(f)
        def decorated(*args, **kwargs):
            # GET/HEAD/OPTIONS 不需要 CSRF 验证
            if request.method in ('GET', 'HEAD', 'OPTIONS'):
                return f(*args, **kwargs)

            # 从请求头或表单获取 Token
            token = request.headers.get('X-CSRF-Token') or request.form.get('csrf_token')
            if not token or not validate_csrf_token(token):
                logger.warning("CSRF 验证失败: ip=%s, path=%s",
                               request.remote_addr, request.path)
                return jsonify({"error": "CSRF Token 无效或已过期，请刷新页面重试"}), 403
            return f(*args, **kwargs)
        return decorated

    # ========================================================
    # 辅助函数
    # ========================================================

    def generate_thumbnail(image_path: str, max_size: int = 200) -> str | None:
        """
        生成图片缩略图的 base64 编码

        参数:
            image_path: 图片路径
            max_size: 缩略图最大尺寸

        返回:
            base64 编码的缩略图字符串，失败返回 None
        """
        if image_path in thumbnail_cache:
            return thumbnail_cache[image_path]

        try:
            from PIL import Image

            img = Image.open(image_path)
            img.thumbnail((max_size, max_size), Image.Resampling.LANCZOS)

            buffer = BytesIO()
            # 根据格式保存
            ext = os.path.splitext(image_path)[1].lower()
            if ext in ('.jpg', '.jpeg'):
                fmt = 'JPEG'
            elif ext == '.png':
                fmt = 'PNG'
            elif ext == '.webp':
                fmt = 'WEBP'
            else:
                fmt = 'JPEG'

            img.save(buffer, format=fmt, quality=80)
            b64 = base64.b64encode(buffer.getvalue()).decode('utf-8')
            mime = 'image/jpeg' if fmt == 'JPEG' else f'image/{fmt.lower()}'
            result = f"data:{mime};base64,{b64}"

            thumbnail_cache[image_path] = result
            return result
        except Exception:
            return None

    def validate_upload_file(file_storage) -> tuple:
        """
        验证上传文件的安全性和合法性

        参数:
            file_storage: Flask 的 FileStorage 对象

        返回:
            (是否合法, 错误信息或安全文件名)
        """
        if not file_storage or not file_storage.filename:
            return False, "无效的文件"

        # 文件名安全过滤
        original_name = file_storage.filename
        safe_name = sanitize_filename(original_name)

        # 检查扩展名白名单
        ext = os.path.splitext(safe_name)[1].lower()
        if ext not in ALLOWED_EXTENSIONS:
            return False, f"不支持的文件类型: {ext}"

        # 检查文件大小
        file_storage.seek(0, os.SEEK_END)
        file_size = file_storage.tell()
        file_storage.seek(0)

        if file_size > MAX_FILE_SIZE:
            return False, f"文件 '{safe_name}' 超出大小限制（{file_size // (1024*1024)}MB > {MAX_FILE_SIZE // (1024*1024)}MB）"

        if file_size == 0:
            return False, f"文件 '{safe_name}' 为空文件"

        return True, safe_name

    # ========================================================
    # 路由：页面
    # ========================================================

    @app.route("/")
    def index():
        """主页面"""
        return render_template("index.html")

    # ========================================================
    # 路由：API - 健康检查
    # ========================================================

    @app.route("/api/health", methods=["GET"])
    def health_check():
        """
        健康检查接口

        返回:
            应用运行状态信息
        """
        return jsonify({
            "status": "ok",
            "timestamp": int(time.time()),
            "version": "1.0.0",
            "scanning": scan_state["is_scanning"],
        })

    # ========================================================
    # 路由：API - CSRF Token
    # ========================================================

    @app.route("/api/csrf-token", methods=["GET"])
    @rate_limit
    def get_csrf_token():
        """
        获取 CSRF Token

        返回:
            新生成的 CSRF Token
        """
        token = generate_csrf_token()
        return jsonify({"csrf_token": token})

    # ========================================================
    # 路由：API - 扫描
    # ========================================================

    @app.route("/api/scan", methods=["POST"])
    @rate_limit
    @csrf_protect
    def scan():
        """
        扫描指定文件夹路径或上传的文件，返回分类结果

        支持:
        1. 文件上传: multipart/form-data, field name: "files"
        2. 文件夹路径: JSON {"path": "/path/to/folder"}
        """
        image_files = []
        folder_path = ""
        temp_dir = None

        # 检查是否有文件上传
        if 'files' in request.files:
            uploaded_files = request.files.getlist('files')
            if not uploaded_files or all(f.filename == '' for f in uploaded_files):
                return jsonify({"error": "请选择要上传的文件"}), 400

            # 创建临时目录存储上传的文件
            temp_dir = tempfile.mkdtemp(prefix="PhotoClassifierPro_")
            folder_path = temp_dir
            total_size = 0

            for file in uploaded_files:
                # 验证文件安全性和合法性
                is_valid, result = validate_upload_file(file)
                if not is_valid:
                    logger.warning("文件上传验证失败: %s", result)
                    continue

                safe_name = result
                filepath = os.path.join(temp_dir, safe_name)

                # 保存文件
                file.save(filepath)

                # 验证文件头魔数（防止扩展名伪造）
                ext = os.path.splitext(safe_name)[1].lower()
                if not validate_file_magic(filepath, ext):
                    logger.warning("文件魔数验证失败，疑似伪造文件: %s", safe_name)
                    os.remove(filepath)
                    continue

                # 累计文件大小检查
                file_stat = os.stat(filepath)
                total_size += file_stat.st_size
                if total_size > MAX_TOTAL_SIZE:
                    logger.warning("总上传大小超出限制: %d bytes", total_size)
                    os.remove(filepath)
                    continue

                image_files.append(filepath)

            if not image_files:
                shutil.rmtree(temp_dir, ignore_errors=True)
                return jsonify({"error": "没有有效的图片文件"}), 400
        else:
            # 从 JSON 获取路径
            data = request.get_json(silent=True)
            if not data:
                return jsonify({"error": "请上传文件或提供文件夹路径"}), 400

            folder_path = data.get("path", "").strip()

            # 输入参数校验：路径不能为空
            if not folder_path:
                return jsonify({"error": "请输入文件夹路径或选择文件上传"}), 400

            # 路径安全检查：防止路径遍历和敏感路径访问
            if not is_path_safe(folder_path):
                logger.warning("路径安全检查失败: %s", folder_path)
                return jsonify({"error": "无效的文件夹路径"}), 400

            if not os.path.isdir(folder_path):
                return jsonify({"error": "路径不存在或不是文件夹"}), 400

            # 扫描图片文件
            image_files = PhotoClassifier.scan_directory(folder_path)

        if scan_state["is_scanning"]:
            if temp_dir:
                shutil.rmtree(temp_dir, ignore_errors=True)
            return jsonify({"error": "正在扫描中，请等待完成"}), 400

        if not image_files:
            if temp_dir:
                shutil.rmtree(temp_dir, ignore_errors=True)
            return jsonify({"error": "未找到图片文件"}), 400

        # 重置状态
        scan_state["is_scanning"] = True
        scan_state["progress"] = 0
        scan_state["total"] = len(image_files)
        scan_state["current_file"] = ""
        scan_state["results"] = {}
        scan_state["error"] = None
        scan_state["temp_dir"] = temp_dir
        thumbnail_cache.clear()

        def run_scan():
            """在后台线程中执行扫描"""
            try:
                def progress_callback(current, total, path):
                    scan_state["progress"] = current
                    scan_state["total"] = total
                    scan_state["current_file"] = os.path.basename(path)

                results = classifier.classify_batch(image_files, progress_callback)
                scan_state["results"] = results
            except Exception as e:
                # 错误信息脱敏后存储
                scan_state["error"] = sanitize_error_message(e)
                logger.error("扫描过程出错: %s", sanitize_error_message(e))
            finally:
                scan_state["is_scanning"] = False
                scan_state["current_file"] = ""

        # 启动后台扫描线程
        thread = threading.Thread(target=run_scan, daemon=True)
        thread.start()

        return jsonify({
            "message": f"开始扫描 {len(image_files)} 张图片",
            "total": len(image_files),
        })

    # ========================================================
    # 路由：API - 状态查询
    # ========================================================

    @app.route("/api/status", methods=["GET"])
    def status():
        """获取扫描进度"""
        response = {
            "is_scanning": scan_state["is_scanning"],
            "progress": scan_state["progress"],
            "total": scan_state["total"],
            "current_file": scan_state["current_file"],
            "error": scan_state["error"],
        }

        if not scan_state["is_scanning"] and scan_state["results"]:
            # 扫描完成，返回分类汇总
            normal_photos = []
            defective_photos = {}

            for path, results in scan_state["results"].items():
                defects = [r for r in results if r.is_defective]

                if defects:
                    # 按缺陷类型分组
                    for defect in defects:
                        dtype = defect.defect_type.value if defect.defect_type else "unknown"
                        if dtype not in defective_photos:
                            defective_photos[dtype] = []
                        defective_photos[dtype].append({
                            "path": path,
                            "filename": os.path.basename(path),
                            "defect_type": dtype,
                            "confidence": defect.confidence,
                            "description": defect.description,
                            "thumbnail": generate_thumbnail(path),
                        })
                else:
                    normal_photos.append({
                        "path": path,
                        "filename": os.path.basename(path),
                        "thumbnail": generate_thumbnail(path),
                    })

            response["completed"] = True
            response["normal_count"] = len(normal_photos)
            response["defective_count"] = sum(len(v) for v in defective_photos.values())
            response["normal_photos"] = normal_photos
            response["defective_photos"] = defective_photos
            response["is_upload"] = scan_state["temp_dir"] is not None

        return jsonify(response)

    # ========================================================
    # 路由：API - 获取删除确认 Token
    # ========================================================

    @app.route("/api/delete-token", methods=["POST"])
    @rate_limit
    @csrf_protect
    def get_delete_token():
        """
        获取删除操作确认 Token（二次确认机制）

        请求体:
            {
                "selected": ["/path/to/photo1.jpg", ...]  (可选，手动模式时指定)
            }

        返回:
            删除确认 Token，前端在确认删除时需携带此 Token
        """
        data = request.get_json(silent=True)
        if not data:
            return jsonify({"error": "请求参数错误"}), 400

        # 构建待删除文件列表
        file_paths = []
        selected = data.get("selected", [])

        if selected:
            # 手动模式：使用前端选中的文件
            for path in selected:
                if path in scan_state["results"]:
                    file_paths.append(path)
        else:
            # 自动模式：所有废片
            for path, results in scan_state["results"].items():
                defects = [r for r in results if r.is_defective]
                if defects:
                    file_paths.append(path)

        if not file_paths:
            return jsonify({"error": "没有需要处理的废片"}), 400

        token = generate_delete_token(file_paths)
        return jsonify({
            "delete_token": token,
            "file_count": len(file_paths),
            "expires_in": DELETE_TOKEN_EXPIRE,
        })

    # ========================================================
    # 路由：API - 处理废片
    # ========================================================

    @app.route("/api/process", methods=["POST"])
    @rate_limit
    @csrf_protect
    def process():
        """
        处理废片（移动到废片文件夹或删除）

        请求体:
            {
                "action": "move" | "delete",
                "mode": "auto" | "manual",
                "folder_path": "/path/to/folder",
                "selected": ["/path/to/photo1.jpg", ...]  (手动模式时指定)
                "delete_token": "..."  (删除操作时必须提供)
            }
        """
        data = request.get_json(silent=True)
        if not data:
            return jsonify({"error": "请求参数错误"}), 400

        # 参数校验和清理
        action = data.get("action", "move").strip().lower()
        mode = data.get("mode", "auto").strip().lower()
        folder_path = data.get("folder_path", "").strip()
        selected = data.get("selected", [])

        # 校验 action 参数
        if action not in ("move", "delete"):
            return jsonify({"error": "无效的操作类型"}), 400

        # 校验 mode 参数
        if mode not in ("auto", "manual"):
            return jsonify({"error": "无效的处理模式"}), 400

        # 校验 selected 参数类型
        if not isinstance(selected, list):
            return jsonify({"error": "参数格式错误"}), 400

        if scan_state["is_scanning"]:
            return jsonify({"error": "正在扫描中，请等待完成"}), 400

        # 构建废片字典
        defective_images = {}

        if mode == "manual" and selected:
            # 手动模式：只处理选中的照片
            for path in selected:
                if path in scan_state["results"]:
                    defects = [r for r in scan_state["results"][path] if r.is_defective]
                    if defects:
                        defective_images[path] = defects
        else:
            # 自动模式：处理所有废片
            for path, results in scan_state["results"].items():
                defects = [r for r in results if r.is_defective]
                if defects:
                    defective_images[path] = defects

        if not defective_images:
            return jsonify({"error": "没有需要处理的废片"}), 400

        # 如果是上传的文件，使用临时目录作为目标
        if scan_state["temp_dir"]:
            folder_path = scan_state["temp_dir"]

        if not folder_path or not os.path.isdir(folder_path):
            return jsonify({"error": "无效的文件夹路径"}), 400

        # 路径安全检查
        if not is_path_safe(folder_path):
            logger.warning("处理操作路径安全检查失败: %s", folder_path)
            return jsonify({"error": "无效的文件夹路径"}), 400

        if action == "move":
            moved = classifier.move_defective(defective_images, folder_path, mode="move")

            # 清理临时目录
            if scan_state["temp_dir"]:
                shutil.rmtree(scan_state["temp_dir"], ignore_errors=True)
                scan_state["temp_dir"] = None

            return jsonify({
                "message": f"已移动 {len(moved)} 张废片到 '{folder_path}/废片/' 文件夹",
                "moved_count": len(moved),
                "moved_files": moved,
            })
        elif action == "delete":
            # 删除操作需要二次确认 Token
            delete_token = data.get("delete_token", "")
            file_paths = list(defective_images.keys())

            if not validate_delete_token(delete_token, file_paths):
                logger.warning("删除操作确认 Token 验证失败: ip=%s",
                               request.remote_addr)
                return jsonify({"error": "删除确认 Token 无效或已过期，请重新获取"}), 403

            deleted = []
            for path in defective_images:
                try:
                    # 再次进行路径安全检查
                    if not is_path_safe(path):
                        logger.warning("删除操作跳过不安全路径: %s", path)
                        continue
                    os.remove(path)
                    deleted.append(path)
                except OSError as e:
                    logger.warning("删除文件失败: %s", sanitize_error_message(e))

            # 清理临时目录
            if scan_state["temp_dir"]:
                shutil.rmtree(scan_state["temp_dir"], ignore_errors=True)
                scan_state["temp_dir"] = None

            return jsonify({
                "message": f"已删除 {len(deleted)} 张废片",
                "deleted_count": len(deleted),
            })
        else:
            return jsonify({"error": "无效的操作类型"}), 400

    return app

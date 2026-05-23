# -*- coding: utf-8 -*-
"""
照片自动分类工具 - Flask Web 端入口

启动方式:
    python run_web.py

启动后访问 http://localhost:5000
"""

import os
import sys

# 将项目根目录加入 Python 路径
PROJECT_ROOT = os.path.dirname(os.path.abspath(__file__))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from web.app import create_app


def main():
    app = create_app()

    print("=" * 50)
    print("  照片自动分类工具 - Web 端")
    print("=" * 50)
    print("  访问地址: http://localhost:5000")
    print("  按 Ctrl+C 停止服务")
    print("=" * 50)

    app.run(host="0.0.0.0", port=5000, debug=False)


if __name__ == "__main__":
    main()

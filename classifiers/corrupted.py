# -*- coding: utf-8 -*-
"""
损坏照片检测器
通过尝试打开和解码图片来判断文件是否损坏
"""

import os
import cv2
import numpy as np
from PIL import Image, UnidentifiedImageError

from .base import BaseDetector, DetectionResult, DefectType


class CorruptedDetector(BaseDetector):
    """损坏照片检测器"""

    @property
    def defect_type(self) -> DefectType:
        return DefectType.CORRUPTED

    def detect(self, image_path: str) -> DetectionResult:
        """
        检测图片是否损坏

        检测流程:
        1. 检查文件是否存在且非空
        2. 检查文件头魔数是否有效
        3. 尝试用 PIL 打开并验证图片
        4. 尝试用 OpenCV 解码图片
        5. 检查解码后的图像数据是否有效
        """
        # 检查文件是否存在
        if not os.path.exists(image_path):
            return DetectionResult(
                is_defective=True,
                defect_type=self.defect_type,
                confidence=1.0,
                description="文件不存在"
            )

        # 检查文件是否为空
        if os.path.getsize(image_path) == 0:
            return DetectionResult(
                is_defective=True,
                defect_type=self.defect_type,
                confidence=1.0,
                description="文件大小为0"
            )

        # 检查文件头魔数
        header_check = self._check_file_header(image_path)
        if not header_check["valid"]:
            return DetectionResult(
                is_defective=True,
                defect_type=self.defect_type,
                confidence=0.95,
                description=f"文件头无效: {header_check['reason']}"
            )

        # 尝试用 PIL 打开图片
        pil_error = self._check_with_pil(image_path)
        if pil_error:
            return DetectionResult(
                is_defective=True,
                defect_type=self.defect_type,
                confidence=0.9,
                description=f"PIL无法打开: {pil_error}"
            )

        # 尝试用 OpenCV 解码图片
        cv_error = self._check_with_opencv(image_path)
        if cv_error:
            return DetectionResult(
                is_defective=True,
                defect_type=self.defect_type,
                confidence=0.9,
                description=f"OpenCV无法解码: {cv_error}"
            )

        # 文件正常
        return DetectionResult(
            is_defective=False,
            defect_type=None,
            confidence=1.0,
            description="文件完整，未检测到损坏"
        )

    def _check_file_header(self, image_path: str) -> dict:
        """
        检查文件头魔数是否为已知图片格式

        返回:
            {"valid": bool, "reason": str}
        """
        # 常见图片格式的文件头魔数
        signatures = {
            b'\xff\xd8\xff': "JPEG",
            b'\x89PNG\r\n\x1a\n': "PNG",
            b'GIF87a': "GIF87a",
            b'GIF89a': "GIF89a",
            b'BM': "BMP",
            b'II*\x00': "TIFF (小端)",
            b'MM\x00*': "TIFF (大端)",
            b'RIFF': "WebP",
        }

        try:
            with open(image_path, 'rb') as f:
                header = f.read(32)

            if not header:
                return {"valid": False, "reason": "文件头为空"}

            for sig, fmt in signatures.items():
                if header.startswith(sig):
                    return {"valid": True, "reason": f"识别为{fmt}格式"}

            # HEIC/HEIF: 基于 ftyp box 验证（box 大小可变，不能靠固定偏移）
            if len(header) >= 12 and header[4:8] == b'ftyp' and header[8:12] in (
                b'mif1', b'msf1', b'heic', b'heix'
            ):
                return {"valid": True, "reason": "识别为HEIC/HEIF格式"}

            return {"valid": False, "reason": "未知文件格式"}
        except IOError as e:
            return {"valid": False, "reason": f"读取文件头失败: {str(e)}"}

    def _check_with_pil(self, image_path: str) -> str | None:
        """
        使用 PIL 打开并验证图片

        返回:
            错误信息字符串，如果正常则返回 None
        """
        try:
            with Image.open(image_path) as img:
                # 强制加载像素数据以验证完整性
                img.load()
                # 检查图像尺寸是否合理
                if img.width <= 0 or img.height <= 0:
                    return f"图像尺寸异常: {img.width}x{img.height}"
            return None
        except UnidentifiedImageError as e:
            return f"无法识别的图片格式: {str(e)}"
        except (IOError, OSError) as e:
            return f"文件读取错误: {str(e)}"
        except Exception as e:
            return f"PIL验证失败: {str(e)}"

    def _check_with_opencv(self, image_path: str) -> str | None:
        """
        使用 OpenCV 解码图片

        返回:
            错误信息字符串，如果正常则返回 None
        """
        try:
            # 使用 imdecode 读取文件内容
            with open(image_path, 'rb') as f:
                file_bytes = np.frombuffer(f.read(), dtype=np.uint8)

            if len(file_bytes) == 0:
                return "文件内容为空"

            img = cv2.imdecode(file_bytes, cv2.IMREAD_COLOR)
            if img is None:
                return "OpenCV无法解码图像数据"

            # 检查解码后的图像数据
            if img.size == 0:
                return "解码后图像数据为空"

            return None
        except Exception as e:
            return f"OpenCV解码异常: {str(e)}"

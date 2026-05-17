# -*- coding: utf-8 -*-
"""
统一分类引擎
整合所有检测器，提供统一的图片分类接口
"""

import os
import cv2
import numpy as np
import shutil
from typing import Callable

from classifiers.base import DetectionResult, DefectType
from classifiers.corrupted import CorruptedDetector
from classifiers.empty import EmptyDetector
from classifiers.blink import BlinkDetector
from classifiers.blur import BlurDetector
from classifiers.obstruction import ObstructionDetector


# 支持的图片扩展名
SUPPORTED_EXTENSIONS = {
    '.jpg', '.jpeg', '.png', '.bmp', '.tiff', '.tif',
    '.webp', '.gif', '.heic', '.heif'
}


class PhotoClassifier:
    """
    照片分类引擎

    整合所有检测器，对单张或多张照片进行缺陷检测和分类。
    """

    def __init__(self):
        """初始化所有检测器"""
        self.detectors = [
            CorruptedDetector(),
            EmptyDetector(),
            BlinkDetector(),
            BlurDetector(),
            ObstructionDetector(),
        ]

    def classify(self, image_path: str) -> list[DetectionResult]:
        """
        对单张图片进行分类检测

        依次运行所有检测器，返回所有检测结果。
        如果文件损坏，直接返回，不运行其他检测器。
        过小的图片跳过人脸相关检测（闭眼、遮挡）。

        参数:
            image_path: 图片文件路径

        返回:
            检测结果列表
        """
        results = []

        # 图像尺寸预检：读取图片尺寸，过小图跳过人脸相关检测
        skip_face_detectors = False
        img_size = self._get_image_size(image_path)
        if img_size is not None:
            h, w = img_size
            if h < 100 or w < 100:
                skip_face_detectors = True

        for detector in self.detectors:
            # 跳过人脸相关检测器（闭眼、遮挡）用于过小图像
            if skip_face_detectors and isinstance(detector, (BlinkDetector, ObstructionDetector)):
                results.append(DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description=f"图像尺寸过小({h}x{w})，跳过{detector.defect_type.value}检测"
                ))
                continue

            result = detector.detect(image_path)
            results.append(result)

            # 如果文件损坏，直接返回，不运行后续检测器
            if result.is_defective and result.defect_type == DefectType.CORRUPTED:
                break

        return results

    @staticmethod
    def _get_image_size(image_path: str) -> tuple | None:
        """快速读取图片尺寸，不解码全部像素数据"""
        try:
            from PIL import Image
            with Image.open(image_path) as img:
                return img.height, img.width
        except Exception:
            pass
        try:
            # 备选：用 OpenCV 只读元数据
            img = cv2.imread(image_path, cv2.IMREAD_UNCHANGED)
            if img is not None:
                return img.shape[:2]
        except Exception:
            pass
        try:
            # 最后备选：仅读取文件头判断
            with open(image_path, 'rb') as f:
                header = f.read(32)
            if header.startswith(b'\xff\xd8'):
                return _jpeg_size_from_header(header)
            if header.startswith(b'\x89PNG'):
                return _png_size_from_header(header)
        except Exception:
            pass
        return None

    def classify_batch(
        self,
        image_paths: list[str],
        callback: Callable[[int, int, str], None] | None = None
    ) -> dict[str, list[DetectionResult]]:
        """
        批量分类图片

        参数:
            image_paths: 图片文件路径列表
            callback: 进度回调函数 callback(current, total, current_path)

        返回:
            字典 {图片路径: 检测结果列表}
        """
        total = len(image_paths)
        results = {}

        for i, image_path in enumerate(image_paths):
            # 回调进度
            if callback:
                callback(i + 1, total, image_path)

            # 执行分类
            detection_results = self.classify(image_path)
            results[image_path] = detection_results

        return results

    def get_defective_images(
        self,
        image_paths: list[str],
        callback: Callable[[int, int, str], None] | None = None
    ) -> dict[str, list[DetectionResult]]:
        """
        批量分类并筛选出有缺陷的图片

        参数:
            image_paths: 图片文件路径列表
            callback: 进度回调函数

        返回:
            字典 {图片路径: 缺陷检测结果列表}（仅包含有缺陷的图片）
        """
        all_results = self.classify_batch(image_paths, callback)
        defective = {}

        for path, results in all_results.items():
            defects = [r for r in results if r.is_defective]
            if defects:
                defective[path] = defects

        return defective

    def move_defective(
        self,
        defective_images: dict[str, list[DetectionResult]],
        target_base_dir: str,
        mode: str = "move"
    ) -> dict[str, str]:
        """
        将废片移动到按类型分类的子文件夹中

        在目标目录下创建 "废片" 子文件夹，按缺陷类型再分子文件夹。

        参数:
            defective_images: 废片字典 {路径: 缺陷列表}
            target_base_dir: 目标基础目录
            mode: "move" 移动文件, "copy" 复制文件

        返回:
            字典 {原路径: 新路径}
        """
        moved_files = {}

        # 缺陷类型对应的中文子文件夹名
        type_folder_names = {
            DefectType.CORRUPTED: "损坏",
            DefectType.EMPTY: "空镜",
            DefectType.BLINK: "闭眼",
            DefectType.BLURRY: "模糊",
            DefectType.OBSTRUCTION: "遮挡",
        }

        for image_path, defects in defective_images.items():
            # 获取主要缺陷类型（取置信度最高的）
            main_defect = max(defects, key=lambda d: d.confidence)
            defect_type = main_defect.defect_type

            if defect_type is None:
                continue

            # 创建目标文件夹
            defect_folder_name = type_folder_names.get(defect_type, "其他")
            target_dir = os.path.join(target_base_dir, "废片", defect_folder_name)
            os.makedirs(target_dir, exist_ok=True)

            # 构建目标路径
            filename = os.path.basename(image_path)
            target_path = os.path.join(target_dir, filename)

            # 处理文件名冲突
            if os.path.exists(target_path) and target_path != image_path:
                name, ext = os.path.splitext(filename)
                counter = 1
                while os.path.exists(target_path):
                    target_path = os.path.join(target_dir, f"{name}_{counter}{ext}")
                    counter += 1

            # 移动或复制文件
            try:
                if mode == "move":
                    shutil.move(image_path, target_path)
                elif mode == "copy":
                    shutil.copy2(image_path, target_path)
                moved_files[image_path] = target_path
            except (OSError, shutil.Error) as e:
                print(f"操作文件失败 {image_path}: {e}")

        return moved_files

    @staticmethod
    def scan_directory(directory: str) -> list[str]:
        """
        扫描目录中的所有图片文件

        参数:
            directory: 要扫描的目录路径

        返回:
            图片文件路径列表（绝对路径）
        """
        image_files = []

        if not os.path.isdir(directory):
            return image_files

        for root, dirs, files in os.walk(directory):
            for filename in files:
                ext = os.path.splitext(filename)[1].lower()
                if ext in SUPPORTED_EXTENSIONS:
                    full_path = os.path.join(root, filename)
                    image_files.append(full_path)

        return image_files


def _jpeg_size_from_header(header: bytes) -> tuple | None:
    """从 JPEG 文件头解析尺寸"""
    try:
        pos = 2
        while pos < len(header) - 1:
            if header[pos] != 0xFF:
                break
            marker = header[pos + 1]
            if marker == 0xC0 or marker == 0xC2:
                height = (header[pos + 5] << 8) | header[pos + 6]
                width = (header[pos + 7] << 8) | header[pos + 8]
                return height, width
            if marker == 0xD9:
                break
            seg_len = (header[pos + 2] << 8) | header[pos + 3]
            pos += seg_len + 2
    except Exception:
        pass
    return None


def _png_size_from_header(header: bytes) -> tuple | None:
    """从 PNG 文件头解析尺寸（IHDR 块）"""
    try:
        if len(header) >= 24:
            width = (header[16] << 24) | (header[17] << 16) | (header[18] << 8) | header[19]
            height = (header[20] << 24) | (header[21] << 16) | (header[22] << 8) | header[23]
            if width > 0 and height > 0:
                return height, width
    except Exception:
        pass
    return None

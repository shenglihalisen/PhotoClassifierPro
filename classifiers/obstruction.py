# -*- coding: utf-8 -*-
"""
遮挡检测器
检测手指遮挡和镜头遮挡
"""

import cv2
import numpy as np
import threading

from .base import BaseDetector, DetectionResult, DefectType


class ObstructionDetector(BaseDetector):
    """
    遮挡检测器

    检测两种类型的遮挡:
    1. 手指遮挡: 在人脸区域周围检测肤色像素异常集中区域
    2. 镜头遮挡: 分析图像四角和边缘区域的颜色分布，大面积异常色块表示遮挡
    """

    # 检测阈值
    CORNER_OBSTRUCTION_RATIO = 0.75   # 四角遮挡比例阈值（3/4 以上才触发）
    CORNER_UNIFORMITY_THRESHOLD = 8   # 四角区域颜色均匀性阈值（标准差，降低以减少误判）
    CORNER_CHECK_SIZE = 0.15          # 四角检查区域占图像尺寸的比例

    # 肤色范围 (HSV) — 兼容浅色与深色皮肤
    SKIN_LOWER = np.array([0, 20, 40], dtype=np.uint8)
    SKIN_UPPER = np.array([25, 200, 255], dtype=np.uint8)

    # 手指遮挡阈值
    FINGER_SKIN_RATIO_THRESHOLD = 0.45        # 整图肤色占比
    FINGER_ABOVE_SKIN_RATIO_THRESHOLD = 0.5   # 人脸上方肤色占比

    # 图像最大边长，超过此值先缩小
    MAX_IMAGE_DIM = 1200

    def __init__(self):
        self._face_cascade = None
        self._cascade_lock = threading.Lock()

    def _get_face_cascade(self):
        if self._face_cascade is None:
            with self._cascade_lock:
                if self._face_cascade is None:
                    self._face_cascade = cv2.CascadeClassifier(
                        cv2.data.haarcascades + 'haarcascade_frontalface_alt2.xml'
                    )
        return self._face_cascade

    @property
    def defect_type(self) -> DefectType:
        return DefectType.OBSTRUCTION

    def detect(self, image_path: str) -> DetectionResult:
        """检测图片是否存在遮挡"""
        try:
            img = self.read_image(image_path)
            if img is None:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description="无法读取图像，跳过遮挡检测"
                )

            # 缩小大图（Haar 在大图上产生大量误检）
            h, w = img.shape[:2]
            if max(w, h) > self.MAX_IMAGE_DIM:
                scale = self.MAX_IMAGE_DIM / max(w, h)
                img = cv2.resize(img, (int(w * scale), int(h * scale)),
                                 interpolation=cv2.INTER_AREA)

            reasons = []
            scores = []

            # 预检查：跳过纯黑/纯白图像（这些由空镜检测器负责）
            gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
            mean_brightness = float(np.mean(gray))
            if mean_brightness < 15 or mean_brightness > 240:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description=f"图像亮度异常(均值={mean_brightness:.1f})，跳过遮挡检测"
                )

            # 1. 镜头遮挡检测（始终执行）
            lens_result = self._detect_lens_obstruction(img)
            if lens_result["is_obstructed"]:
                scores.append(lens_result["confidence"])
                reasons.append(lens_result["description"])

            # 2. 手指遮挡检测（需要人脸）
            finger_result = self._detect_finger_obstruction(img)
            if finger_result["is_obstructed"]:
                scores.append(finger_result["confidence"])
                reasons.append(finger_result["description"])

            # 综合判断
            if scores:
                max_confidence = max(scores)
                return DetectionResult(
                    is_defective=True,
                    defect_type=self.defect_type,
                    confidence=max_confidence,
                    description=f"遮挡检测: {'; '.join(reasons)}"
                )
            else:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.8,
                    description="未检测到遮挡"
                )

        except Exception as e:
            return DetectionResult(
                is_defective=False,
                defect_type=None,
                confidence=0.0,
                description=f"遮挡检测异常: {str(e)}"
            )

    def _detect_lens_obstruction(self, img: np.ndarray) -> dict:
        """
        检测镜头遮挡

        分析图像四角和边缘区域，如果存在大面积均匀色块（如手指遮住镜头），
        则判定为遮挡。

        返回:
            {"is_obstructed": bool, "confidence": float, "description": str}
        """
        h, w = img.shape[:2]
        corner_size_w = int(w * self.CORNER_CHECK_SIZE)
        corner_size_h = int(h * self.CORNER_CHECK_SIZE)

        # 提取四个角落区域
        corners = [
            img[:corner_size_h, :corner_size_w],                          # 左上
            img[:corner_size_h, w - corner_size_w:],                      # 右上
            img[h - corner_size_h:, :corner_size_w],                      # 左下
            img[h - corner_size_h:, w - corner_size_w:],                  # 右下
        ]

        # 提取四条边缘区域
        edge_thickness = max(3, int(min(h, w) * 0.03))
        edges = [
            img[:edge_thickness, :],          # 上边缘
            img[h - edge_thickness:, :],      # 下边缘
            img[:, :edge_thickness],          # 左边缘
            img[:, w - edge_thickness:],      # 右边缘
        ]

        # 计算中心区域的平均颜色（用于与四角对比）
        center_region = img[h//4:3*h//4, w//4:3*w//4]
        center_mean_color = center_region.mean(axis=(0, 1))

        obstructed_corners = 0

        # 检查四角区域
        for i, corner in enumerate(corners):
            # 计算该区域的颜色标准差
            std_b = float(np.std(corner[:, :, 0]))
            std_g = float(np.std(corner[:, :, 1]))
            std_r = float(np.std(corner[:, :, 2]))
            avg_std = (std_b + std_g + std_r) / 3.0

            # 计算该区域的平均颜色
            mean_color = corner.mean(axis=(0, 1))

            # 计算四角与中心的颜色差异
            color_diff = float(np.abs(mean_color - center_mean_color).mean())

            # 遮挡判定：四角自身均匀 + 与中心颜色差异大
            is_uniform = avg_std < self.CORNER_UNIFORMITY_THRESHOLD
            is_different_from_center = color_diff > 25  # 与中心差异超过阈值

            if is_uniform and is_different_from_center:
                obstructed_corners += 1

        # 检查边缘区域
        obstructed_edges = 0
        for edge in edges:
            std_b = float(np.std(edge[:, :, 0]))
            std_g = float(np.std(edge[:, :, 1]))
            std_r = float(np.std(edge[:, :, 2]))
            avg_std = (std_b + std_g + std_r) / 3.0

            edge_mean_color = edge.mean(axis=(0, 1))
            color_diff = float(np.abs(edge_mean_color - center_mean_color).mean())

            is_uniform = avg_std < self.CORNER_UNIFORMITY_THRESHOLD
            is_different_from_center = color_diff > 25

            if is_uniform and is_different_from_center:
                obstructed_edges += 1

        # 判断逻辑：多个角落被遮挡 + 边缘也有遮挡
        corner_ratio = obstructed_corners / 4.0
        edge_ratio = obstructed_edges / 4.0

        is_obstructed = corner_ratio >= self.CORNER_OBSTRUCTION_RATIO and edge_ratio >= 0.5

        if is_obstructed:
            confidence = min(1.0, corner_ratio + edge_ratio * 0.3)
            return {
                "is_obstructed": True,
                "confidence": confidence,
                "description": f"镜头遮挡: {obstructed_corners}/4个角落异常, "
                              f"{obstructed_edges}/4条边缘异常"
            }

        return {
            "is_obstructed": False,
            "confidence": 0.0,
            "description": ""
        }

    def _detect_finger_obstruction(self, img: np.ndarray) -> dict:
        """
        检测手指遮挡

        在图像中检测肤色像素，如果在人脸区域周围或图像边缘
        存在大面积肤色像素集中区域，可能是手指遮挡。

        返回:
            {"is_obstructed": bool, "confidence": float, "description": str}
        """
        try:
            h, w = img.shape[:2]

            if h < 60 or w < 60:
                return {"is_obstructed": False, "confidence": 0.0, "description": ""}

            gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
            face_cascade = self._get_face_cascade()

            # 检测人脸
            faces = face_cascade.detectMultiScale(
                gray, scaleFactor=1.1, minNeighbors=5, minSize=(60, 60)
            )

            # 转换为 HSV 检测肤色
            hsv = cv2.cvtColor(img, cv2.COLOR_BGR2HSV)
            skin_mask = cv2.inRange(hsv, self.SKIN_LOWER, self.SKIN_UPPER)

            kernel = cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (5, 5))
            skin_mask = cv2.morphologyEx(skin_mask, cv2.MORPH_CLOSE, kernel, iterations=2)
            skin_mask = cv2.morphologyEx(skin_mask, cv2.MORPH_OPEN, kernel, iterations=1)

            total_skin_pixels = cv2.countNonZero(skin_mask)
            total_pixels = h * w
            skin_ratio = total_skin_pixels / total_pixels

            if skin_ratio > self.FINGER_SKIN_RATIO_THRESHOLD:
                # 特写人脸也会肤色占比高，排除人脸占比较大的情况
                face_ratio = 0.0
                for (fx2, fy2, fw2, fh2) in faces:
                    face_ratio += fw2 * fh2
                face_ratio /= (h * w)

                if face_ratio < 0.15:
                    confidence = min(1.0, (skin_ratio - self.FINGER_SKIN_RATIO_THRESHOLD) / 0.2 + 0.5)
                    return {
                        "is_obstructed": True,
                        "confidence": confidence,
                        "description": f"手指遮挡: 肤色占比={skin_ratio:.1%} (阈值={self.FINGER_SKIN_RATIO_THRESHOLD:.0%})"
                    }

            # 检查人脸上方区域的肤色集中（排除特写脸—额头也是肤色）
            for (fx2, fy2, fw2, fh2) in faces:
                if fw2 * fh2 > h * w * 0.30:
                    continue
                above_face_y_start = max(0, fy2 - int(fh2 * 0.5))
                above_face_y_end = fy2
                above_face_x_start = max(0, fx2 - 20)
                above_face_x_end = min(w, fx2 + fw2 + 20)

                if above_face_y_end > above_face_y_start:
                    above_region = skin_mask[above_face_y_start:above_face_y_end,
                                             above_face_x_start:above_face_x_end]
                    above_region_size = above_region.size
                    if above_region_size > 0:
                        above_skin_ratio = cv2.countNonZero(above_region) / above_region_size
                        if above_skin_ratio > self.FINGER_ABOVE_SKIN_RATIO_THRESHOLD:
                            confidence = min(1.0, above_skin_ratio)
                            return {
                                "is_obstructed": True,
                                "confidence": confidence,
                                "description": f"手指遮挡: 人脸上方肤色集中(占比={above_skin_ratio:.1%})"
                            }

            return {"is_obstructed": False, "confidence": 0.0, "description": ""}

        except Exception as e:
            import logging
            logging.getLogger("PhotoClassifierPro").warning(
                "手指遮挡检测异常: %s", str(e))
            return {"is_obstructed": False, "confidence": 0.0, "description": ""}

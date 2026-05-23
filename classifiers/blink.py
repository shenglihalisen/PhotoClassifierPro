# -*- coding: utf-8 -*-
"""
闭眼检测器
使用 Haar 级联检测人脸，基于人脸几何定位眼部区域，
通过 Canny 边缘密度分析判断眼睛是否闭合。
"""

import cv2
import numpy as np
import threading

from .base import BaseDetector, DetectionResult, DefectType


class BlinkDetector(BaseDetector):
    """
    闭眼检测器

    流程:
      1. Haar Cascade 检测正面人脸
      2. 根据人脸几何裁剪左/右眼区域
      3. Canny 边缘检测，计算边缘像素密度
      4. 闭眼时虹膜消失，眼部边缘显著减少
    """

    # 眼部区域 Canny 边缘密度阈值（低于此值判定为闭眼）
    EDGE_DENSITY_THRESHOLD = 0.14

    # 人脸最小尺寸（像素）
    MIN_FACE_SIZE = 60

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
        return DefectType.BLINK

    def _compute_edge_density(self, roi: np.ndarray) -> float:
        """计算 Canny 边缘密度（边缘像素比例）"""
        if roi.size < 100:
            return 0.0
        edges = cv2.Canny(roi, 30, 100)
        return float(np.mean(edges > 0))

    def _extract_eye_region(self, face_roi: np.ndarray, left: bool) -> np.ndarray:
        """根据人脸几何提取左眼或右眼区域"""
        h, w = face_roi.shape[:2]
        if left:
            x_start, x_end = int(w * 0.10), int(w * 0.42)
        else:
            x_start, x_end = int(w * 0.58), int(w * 0.90)
        y_start, y_end = int(h * 0.22), int(h * 0.48)
        return face_roi[y_start:y_end, x_start:x_end]

    def detect(self, image_path: str) -> DetectionResult:
        """检测图片中是否有人闭眼"""
        try:
            img = self.read_image(image_path)
            if img is None:
                return DetectionResult(
                    is_defective=False, defect_type=None, confidence=0.0,
                    description="无法读取图像，跳过闭眼检测"
                )

            h, w = img.shape[:2]
            if max(w, h) > self.MAX_IMAGE_DIM:
                scale = self.MAX_IMAGE_DIM / max(w, h)
                img = cv2.resize(img, (int(w * scale), int(h * scale)),
                                 interpolation=cv2.INTER_AREA)

            gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
            face_cascade = self._get_face_cascade()

            faces = face_cascade.detectMultiScale(
                gray, scaleFactor=1.1, minNeighbors=5,
                minSize=(self.MIN_FACE_SIZE, self.MIN_FACE_SIZE)
            )

            if len(faces) == 0:
                return DetectionResult(
                    is_defective=False, defect_type=None, confidence=0.0,
                    description="未检测到人脸，跳过闭眼检测"
                )

            face_count = len(faces)
            blink_faces = []
            overall_min_density = 1.0

            for face_idx, (fx, fy, fw, fh) in enumerate(faces):
                face_roi = gray[fy:fy + fh, fx:fx + fw]

                left_eye = self._extract_eye_region(face_roi, left=True)
                right_eye = self._extract_eye_region(face_roi, left=False)

                if left_eye.size == 0 or right_eye.size == 0:
                    continue

                # 计算眼部边缘密度（闭眼时虹膜消失，边缘显著减少）
                left_density = self._compute_edge_density(left_eye)
                right_density = self._compute_edge_density(right_eye)

                eye_density = min(left_density, right_density)
                overall_min_density = min(overall_min_density, eye_density)

                if eye_density < self.EDGE_DENSITY_THRESHOLD:
                    blink_faces.append(face_idx + 1)

            if blink_faces:
                confidence = min(1.0, (self.EDGE_DENSITY_THRESHOLD - overall_min_density)
                                 / self.EDGE_DENSITY_THRESHOLD)
                return DetectionResult(
                    is_defective=True,
                    defect_type=self.defect_type,
                    confidence=confidence,
                    description=f"检测到{face_count}张人脸，第{blink_faces}张人脸闭眼 "
                               f"(最小边缘密度={overall_min_density:.4f}, 阈值={self.EDGE_DENSITY_THRESHOLD})"
                )
            else:
                return DetectionResult(
                    is_defective=False, defect_type=None, confidence=1.0,
                    description=f"检测到{face_count}张人脸，均未闭眼 "
                               f"(最小边缘密度={overall_min_density:.4f})"
                )

        except Exception as e:
            return DetectionResult(
                is_defective=False, defect_type=None, confidence=0.0,
                description=f"闭眼检测异常: {str(e)}"
            )

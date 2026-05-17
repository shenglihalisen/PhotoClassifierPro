# -*- coding: utf-8 -*-
"""
模糊检测器
使用拉普拉斯算子和 Sobel 算子计算图像清晰度
"""

import cv2
import numpy as np

from .base import BaseDetector, DetectionResult, DefectType


class BlurDetector(BaseDetector):
    """
    模糊检测器

    使用两种方法综合判断图像是否模糊:
    1. 拉普拉斯方差: 衡量图像二阶导数强度，值越低越模糊
    2. Sobel 算子: 计算边缘强度，边缘越少越模糊
    """

    # 检测阈值
    LAPLACIAN_THRESHOLD = 60       # 拉普拉斯方差阈值
    SOBEL_THRESHOLD = 35           # Sobel 边缘均值阈值
    BLUR_CONFIDENCE_THRESHOLD = 0.5  # 综合置信度阈值

    @property
    def defect_type(self) -> DefectType:
        return DefectType.BLURRY

    def detect(self, image_path: str) -> DetectionResult:
        """检测图片是否模糊"""
        try:
            img = self.read_image(image_path)
            if img is None:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description="无法读取图像，跳过模糊检测"
                )

            # 转换为灰度图
            gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)

            # 跳过纯黑/纯白图像（这些由空镜检测器负责）
            mean_brightness = float(np.mean(gray))
            if mean_brightness < 15 or mean_brightness > 240:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description=f"图像亮度异常(均值={mean_brightness:.1f})，跳过模糊检测"
                )

            # 1. 拉普拉斯方差检测
            laplacian_var = self._calculate_laplacian_variance(gray)

            # 2. Sobel 边缘检测
            sobel_score = self._calculate_sobel_score(gray)

            # 综合评分
            laplacian_score = self._normalize_laplacian_score(laplacian_var)
            sobel_score_normalized = self._normalize_sobel_score(sobel_score)

            # 加权综合（拉普拉斯权重更高）
            overall_confidence = 0.6 * laplacian_score + 0.4 * sobel_score_normalized

            if overall_confidence >= self.BLUR_CONFIDENCE_THRESHOLD:
                reasons = []
                if laplacian_var < self.LAPLACIAN_THRESHOLD:
                    reasons.append(f"拉普拉斯方差={laplacian_var:.1f} (阈值={self.LAPLACIAN_THRESHOLD})")
                if sobel_score < self.SOBEL_THRESHOLD:
                    reasons.append(f"Sobel边缘均值={sobel_score:.1f} (阈值={self.SOBEL_THRESHOLD})")

                return DetectionResult(
                    is_defective=True,
                    defect_type=self.defect_type,
                    confidence=min(1.0, overall_confidence),
                    description=f"模糊检测: {'; '.join(reasons)} (综合置信度={overall_confidence:.2f})"
                )
            else:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=1.0 - overall_confidence,
                    description=f"图像清晰 (拉普拉斯方差={laplacian_var:.1f}, "
                               f"Sobel边缘均值={sobel_score:.1f})"
                )

        except Exception as e:
            return DetectionResult(
                is_defective=False,
                defect_type=None,
                confidence=0.0,
                description=f"模糊检测异常: {str(e)}"
            )

    def _calculate_laplacian_variance(self, gray: np.ndarray) -> float:
        """
        计算拉普拉斯方差

        拉普拉斯算子是二阶导数，对边缘和细节敏感。
        方差越低，图像越模糊。
        """
        laplacian = cv2.Laplacian(gray, cv2.CV_64F)
        return float(laplacian.var())

    def _calculate_sobel_score(self, gray: np.ndarray) -> float:
        """
        计算 Sobel 边缘强度均值

        分别计算水平和垂直方向的 Sobel 梯度，
        取梯度幅值的均值作为边缘强度指标。
        """
        sobel_x = cv2.Sobel(gray, cv2.CV_64F, 1, 0, ksize=3)
        sobel_y = cv2.Sobel(gray, cv2.CV_64F, 0, 1, ksize=3)

        # 计算梯度幅值
        gradient_magnitude = np.sqrt(sobel_x ** 2 + sobel_y ** 2)
        return float(np.mean(gradient_magnitude))

    def _normalize_laplacian_score(self, laplacian_var: float) -> float:
        """
        将拉普拉斯方差归一化为模糊置信度 (0.0 ~ 1.0)

        方差越低，置信度越高（越模糊）
        """
        if laplacian_var >= self.LAPLACIAN_THRESHOLD:
            # 高于阈值，线性衰减
            score = max(0.0, 1.0 - (laplacian_var - self.LAPLACIAN_THRESHOLD) / self.LAPLACIAN_THRESHOLD)
            return score * 0.5  # 高于阈值时最多给 0.5
        else:
            # 低于阈值，线性增长
            return min(1.0, 1.0 - laplacian_var / self.LAPLACIAN_THRESHOLD)

    def _normalize_sobel_score(self, sobel_score: float) -> float:
        """
        将 Sobel 边缘均值归一化为模糊置信度 (0.0 ~ 1.0)

        边缘强度越低，置信度越高（越模糊）
        """
        if sobel_score >= self.SOBEL_THRESHOLD:
            score = max(0.0, 1.0 - (sobel_score - self.SOBEL_THRESHOLD) / self.SOBEL_THRESHOLD)
            return score * 0.5
        else:
            return min(1.0, 1.0 - sobel_score / self.SOBEL_THRESHOLD)

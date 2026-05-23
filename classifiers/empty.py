# -*- coding: utf-8 -*-
"""
空镜/无内容检测器
通过分析图像亮度、纹理和色彩变化来判断是否为空镜
"""

import cv2
import numpy as np

from .base import BaseDetector, DetectionResult, DefectType


class EmptyDetector(BaseDetector):
    """
    空镜/无内容检测器

    判断标准:
    1. 灰度均值：纯黑(< 15) 或 纯白(> 240)
    2. 拉普拉斯方差：极低值表示无纹理
    3. 颜色标准差：极低值表示缺乏色彩变化
    4. 综合评分低于阈值判定为空镜
    """

    # 检测阈值
    BLACK_THRESHOLD = 15          # 纯黑灰度阈值
    WHITE_THRESHOLD = 240         # 纯白灰度阈值
    LAPLACIAN_VAR_THRESHOLD = 10  # 拉普拉斯方差阈值（无纹理）
    COLOR_STD_THRESHOLD = 8       # 颜色标准差阈值（无色彩变化）
    OVERALL_SCORE_THRESHOLD = 0.6 # 综合评分阈值

    @property
    def defect_type(self) -> DefectType:
        return DefectType.EMPTY

    def detect(self, image_path: str) -> DetectionResult:
        """检测图片是否为空镜"""
        try:
            img = self.read_image(image_path)
            if img is None:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=0.0,
                    description="无法读取图像，跳过空镜检测"
                )

            # 转换为灰度图
            gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)

            # 1. 计算灰度均值
            mean_brightness = float(np.mean(gray))

            # 2. 计算拉普拉斯方差（纹理丰富度）
            laplacian_var = float(cv2.Laplacian(gray, cv2.CV_64F).var())

            # 3. 计算颜色标准差（色彩丰富度）
            color_std = self._calculate_color_std(img)

            # 4. 计算灰度直方图分布
            hist_score = self._calculate_histogram_score(gray)

            # 综合评分
            scores = []
            reasons = []

            # 亮度异常评分
            if mean_brightness < self.BLACK_THRESHOLD:
                scores.append(1.0)
                reasons.append(f"纯黑图像(均值={mean_brightness:.1f})")
            elif mean_brightness > self.WHITE_THRESHOLD:
                scores.append(1.0)
                reasons.append(f"纯白图像(均值={mean_brightness:.1f})")
            elif mean_brightness < 30:
                scores.append(0.7)
                reasons.append(f"严重欠曝(均值={mean_brightness:.1f})")
            elif mean_brightness > 225:
                scores.append(0.7)
                reasons.append(f"严重过曝(均值={mean_brightness:.1f})")
            else:
                scores.append(0.0)

            # 纹理缺失评分
            if laplacian_var < self.LAPLACIAN_VAR_THRESHOLD:
                texture_score = 1.0 - (laplacian_var / self.LAPLACIAN_VAR_THRESHOLD)
                scores.append(texture_score)
                reasons.append(f"无纹理内容(拉普拉斯方差={laplacian_var:.1f})")
            else:
                scores.append(0.0)

            # 色彩缺失评分
            if color_std < self.COLOR_STD_THRESHOLD:
                color_score = 1.0 - (color_std / self.COLOR_STD_THRESHOLD)
                scores.append(color_score)
                reasons.append(f"缺乏色彩变化(颜色标准差={color_std:.1f})")
            else:
                scores.append(0.0)

            # 直方图集中度评分
            scores.append(hist_score)
            if hist_score > 0.5:
                reasons.append(f"直方图过于集中(评分={hist_score:.2f})")

            # 计算综合评分
            overall_score = float(np.mean(scores))

            if overall_score >= self.OVERALL_SCORE_THRESHOLD:
                confidence = min(1.0, overall_score)
                return DetectionResult(
                    is_defective=True,
                    defect_type=self.defect_type,
                    confidence=confidence,
                    description=f"空镜检测: {'; '.join(reasons)} (综合评分={overall_score:.2f})"
                )
            else:
                return DetectionResult(
                    is_defective=False,
                    defect_type=None,
                    confidence=1.0 - overall_score,
                    description=f"非空镜 (综合评分={overall_score:.2f})"
                )

        except Exception as e:
            return DetectionResult(
                is_defective=False,
                defect_type=None,
                confidence=0.0,
                description=f"空镜检测异常: {str(e)}"
            )

    def _calculate_color_std(self, img: np.ndarray) -> float:
        """
        计算图像的颜色标准差

        分别计算 B、G、R 三个通道的标准差，取平均值
        标准差越低表示色彩变化越少
        """
        std_b = float(np.std(img[:, :, 0]))
        std_g = float(np.std(img[:, :, 1]))
        std_r = float(np.std(img[:, :, 2]))
        return (std_b + std_g + std_r) / 3.0

    def _calculate_histogram_score(self, gray: np.ndarray) -> float:
        """
        计算灰度直方图集中度评分

        如果像素值高度集中在某个狭窄区间，说明图像内容单调
        返回 0.0 ~ 1.0 的评分，越高表示越集中（越可能是空镜）
        """
        # 计算灰度直方图
        hist = cv2.calcHist([gray], [0], None, [256], [0, 256])
        hist = hist.flatten()
        total_pixels = gray.size

        if total_pixels == 0:
            return 0.0

        # 归一化直方图
        hist_norm = hist / total_pixels

        # 找到直方图中占比最高的连续区间
        window_size = 30  # 窗口大小
        max_concentration = 0.0

        for i in range(256 - window_size + 1):
            concentration = float(np.sum(hist_norm[i:i + window_size]))
            max_concentration = max(max_concentration, concentration)

        # 如果超过 85% 的像素集中在 30 级灰度范围内，认为内容单调
        if max_concentration > 0.85:
            return (max_concentration - 0.85) / 0.15  # 映射到 0~1
        return 0.0

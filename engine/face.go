package engine

// face.go 人脸检测器的注入点。
//
// 为什么用注入而不是直接 import face：qc 包定义了 FaceDetector 接口，
// engine 只需要一个「造检测器」的工厂。这样 engine/qc 都不依赖 face 的具体实现，
// 只有 main 把它们接起来，测试时可以塞假实现。
import "github.com/shenglihalisen/PhotoClassifierPro/qc"

var factory func() qc.FaceDetector

// SetFaceFactory 注册人脸检测器工厂。传 nil 表示关闭闭眼检测。
func SetFaceFactory(f func() qc.FaceDetector) { factory = f }

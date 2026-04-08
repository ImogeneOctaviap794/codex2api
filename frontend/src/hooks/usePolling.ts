import { useEffect, useRef } from 'react'

/**
 * usePolling — 统一的前端轮询 hook。
 *
 * - 仅在 document.visibilityState === 'visible' 时触发回调
 * - 页面切换到后台时自动暂停，回到前台时立即触发一次
 * - 组件卸载自动清理
 */
export function usePolling(
  callback: () => void | Promise<void>,
  intervalMs: number,
  enabled = true,
) {
  const savedCallback = useRef(callback)
  savedCallback.current = callback

  useEffect(() => {
    if (!enabled || intervalMs <= 0) return

    const tick = () => {
      if (document.visibilityState === 'visible') {
        void savedCallback.current()
      }
    }

    const timer = window.setInterval(tick, intervalMs)

    // 从后台切回前台时立即刷新一次
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') {
        void savedCallback.current()
      }
    }
    document.addEventListener('visibilitychange', onVisibilityChange)

    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [intervalMs, enabled])
}

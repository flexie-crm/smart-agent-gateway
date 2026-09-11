import { Component, type ErrorInfo, type ReactNode } from 'react';

interface Props {
  children: ReactNode;
  fallback?: ReactNode;
}

interface State {
  hasError: boolean;
  error: Error | null;
}

/**
 * Catches a rendering error in the conversation without taking the chat with it.
 *
 * Two things about the way this fails are as important as the catching, and both
 * were wrong. It sat around a region that FILLS the window, and it replaced that
 * region with a small centred box, so the layout collapsed and the composer flew
 * to the top of the screen: the failure looked far worse than it was, and it did
 * not look like the application people had been using a second earlier. And it
 * announced itself in the middle of the space the conversation had occupied, as
 * though the conversation were gone.
 *
 * It is not gone. The transcript lives in the hook above this, outside what is
 * being caught, so the messages survive and Try again brings them straight back.
 * So: the region keeps its size and shape, the notice is a strip rather than a
 * takeover, and it sits at the BOTTOM, next to the composer, where the newest
 * thing is and where the eye already is.
 */
export class ChatErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('[ChatErrorBoundary] Rendering error:', error, info.componentStack);
  }

  render() {
    if (this.state.hasError) {
      if (this.props.fallback) return this.props.fallback;

      return (
        // The same shape the conversation had: filling what it filled, so
        // nothing else on the screen moves.
        <div className="flex min-h-0 flex-1 flex-col justify-end">
          <div className="mx-auto mb-3 w-full max-w-[776px] px-4">
            <div className="flex items-center gap-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm dark:border-amber-900/50 dark:bg-amber-950/40">
              <span className="flex-1 text-amber-900 dark:text-amber-200">
                This conversation could not be drawn. Your messages are safe.
              </span>
              <button
                onClick={() => this.setState({ hasError: false, error: null })}
                className="shrink-0 rounded-md border border-amber-300 px-2.5 py-1 text-xs text-amber-900 hover:bg-amber-100 dark:border-amber-800 dark:text-amber-200 dark:hover:bg-amber-900/40"
              >
                Show it again
              </button>
            </div>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}

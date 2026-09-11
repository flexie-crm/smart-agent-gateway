import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import hardenReactMarkdown from 'harden-react-markdown'

/**
 * A document, rendered.
 *
 * It is HARDENED, and that is not paranoia: an AGENT writes into these
 * documents. Whatever a model can be talked into writing must not be able to
 * become a script tag, or a link to somewhere nobody chose, in the browser of
 * the administrator who opens it next.
 *
 * The content is stored as Markdown and only ever becomes HTML here, in the
 * browser, behind that guard. The server never renders it.
 */
const Hardened = hardenReactMarkdown(ReactMarkdown)

export function Markdown({ children }: { children: string }) {
  return (
    <div className="prose-sag">
      <Hardened
        remarkPlugins={[remarkGfm]}
        // Nothing is trusted by default. A document may link out, and the link
        // opens in its own tab with no window handle back to this one.
        defaultOrigin={window.location.origin}
        allowedLinkPrefixes={['https://', 'http://', 'mailto:']}
        allowedImagePrefixes={['https://', 'http://']}
        components={{
          a: ({ children, ...props }) => (
            <a {...props} target="_blank" rel="noopener noreferrer">
              {children}
            </a>
          ),
        }}
      >
        {children}
      </Hardened>
    </div>
  )
}

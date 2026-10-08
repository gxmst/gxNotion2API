import { memo } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import remarkCjkFriendly from 'remark-cjk-friendly/parseOnly';
import remarkGfm from 'remark-gfm';
import { safeMarkdownComponents } from '@/components/admin/safe-markdown';

// CommonMark otherwise leaves **中文标点。**正文 literal. Fix delimiter
// parsing without rewriting the reply, escaped markers, or code examples.
const plugins = [remarkGfm, remarkCjkFriendly];
const components: Components = {
  ...safeMarkdownComponents,
  table: ({ children }) => <div className="markdown-table"><table>{children}</table></div>,
};

export const MessageMarkdown = memo(function MessageMarkdown({ content }: { content: string }) {
  return <div className="message-markdown">
    <ReactMarkdown remarkPlugins={plugins} components={components}>{content}</ReactMarkdown>
  </div>;
});

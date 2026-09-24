import type { Components } from 'react-markdown';

const EXTERNAL_REL = 'noopener noreferrer nofollow';

// Model output is untrusted: an auto-loaded image URL can carry chat content
// to a third-party server (prompt-injection exfiltration). Images are rendered
// as plain links so nothing is fetched until the user explicitly clicks.
export const safeMarkdownComponents: Components = {
  a: ({ children, href }) => <a href={href} target="_blank" rel={EXTERNAL_REL}>{children}</a>,
  img: ({ src, alt }) => {
    const href = typeof src === 'string' ? src : undefined;
    const label = alt?.trim() || href || '图片';
    return href ? <a href={href} target="_blank" rel={EXTERNAL_REL} title={href}>[图片：{label}]</a> : <span>[图片：{label}]</span>;
  },
};

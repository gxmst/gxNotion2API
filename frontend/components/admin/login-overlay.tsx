'use client';
import { useEffect, useState } from 'react';
import { LockKeyhole } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

export function LoginOverlay({ passwordConfigured, message, busy, onSubmit }: {
  passwordConfigured: boolean; message: string; busy: boolean;
  onSubmit: (password: string) => Promise<void>;
}) {
  const [password, setPassword] = useState('');
  const [localMessage, setLocalMessage] = useState('');
  useEffect(() => { setLocalMessage(message); }, [message]);
  return <div className="fixed inset-0 z-50 overflow-y-auto bg-background">
    <div className="flex min-h-dvh items-center justify-center px-5 py-10">
      <div className="w-full max-w-sm space-y-7">
        <div className="app-brand justify-center"><span>N</span><div>Notion2API<small>你的 AI 工作空间</small></div></div>
        <section className="panel-card p-7 space-y-5" aria-label="登录">
          <LockKeyhole className="size-6 text-primary" />
          <div><h1 className="text-xl font-semibold">{passwordConfigured ? '欢迎回来' : '设置管理密码'}</h1>
            <p className="text-sm text-muted-foreground mt-2 leading-6">{passwordConfigured ? '登录后继续聊天，或管理账号与工作区。' : '请先在服务配置中设置 admin.password，再重新加载页面。'}</p>
          </div>
          {passwordConfigured ? <form className="space-y-4" onSubmit={async (event) => {
            event.preventDefault(); setLocalMessage('');
            try { await onSubmit(password); setPassword(''); }
            catch (error) { setLocalMessage(error instanceof Error ? error.message : '登录失败'); }
          }}>
            <label htmlFor="admin-password" className="block text-sm">管理密码</label>
            <Input id="admin-password" type="password" autoComplete="current-password" autoFocus value={password}
              onChange={(event) => setPassword(event.target.value)} placeholder="请输入管理密码" disabled={busy} />
            <Button type="submit" disabled={busy || !password.trim()} className="w-full">{busy ? '登录中…' : '登录控制台'}</Button>
          </form> : <Button className="w-full" onClick={() => window.location.reload()}>重新加载</Button>}
          {localMessage ? <p role="status" className="text-xs leading-6 text-muted-foreground">{localMessage}</p> : null}
        </section>
        <p className="text-center text-xs text-muted-foreground">聊天 · 工作区 · API</p>
      </div>
    </div>
  </div>;
}

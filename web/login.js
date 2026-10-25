'use strict';

const form = document.getElementById('login-form');
const input = document.getElementById('login-password');
const errorBox = document.getElementById('login-error');
const submit = document.getElementById('login-submit');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.classList.add('hidden');
  submit.disabled = true;
  submit.textContent = '登录中…';
  try {
    const response = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: input.value }),
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(body.error?.message || body.error || `登录失败（HTTP ${response.status}）`);
    }
    window.location.replace('/');
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
    input.select();
  } finally {
    submit.disabled = false;
    submit.textContent = '登录';
  }
});
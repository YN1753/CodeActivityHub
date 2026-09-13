/** @type {import('tailwindcss').Config} */
export default {
  // 只扫模板与脚本，产物交给 src/style.css 里的设计令牌
  content: ['./index.html', './src/**/*.{js,vue}'],
  theme: {},
  plugins: []
};
